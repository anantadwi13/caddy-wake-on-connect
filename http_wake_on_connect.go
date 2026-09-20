package caddy_wake_on_connect

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func init() {
	caddy.RegisterModule(HTTPWakeOnConnect{})
}

var defaultKeyPaths = []string{
	"~/.ssh/id_ed25519",
	"~/.ssh/id_rsa",
}

type HTTPWakeOnConnect struct {
	reverseproxy.HTTPTransport
	WOCSSHUser            string         `json:"woc_ssh_user,omitempty"`              // mandatory
	WOCSSHAddress         string         `json:"woc_ssh_address,omitempty"`           // mandatory
	WOCSSHKeyFiles        string         `json:"woc_ssh_key_files,omitempty"`         // default: ~/.ssh/id_ed25519 or ~/.ssh/id_rsa, support multiple fallback keys separated by comma
	WOCSSHKnownHostsFiles string         `json:"woc_ssh_known_hosts_files,omitempty"` // default: ~/.ssh/known_hosts, support multiple files separated by comma
	WOCSSHCommand         string         `json:"woc_ssh_command,omitempty"`           // default: tail -f /dev/null
	WOCSSHTimeout         caddy.Duration `json:"woc_ssh_timeout,omitempty"`           // default: 30s
	WOCSSHKeepAlive       caddy.Duration `json:"woc_ssh_keep_alive,omitempty"`        // default: 30s
	WOCSSHPollingInterval caddy.Duration `json:"woc_ssh_polling_interval,omitempty"`  // default: 250ms

	id                  string
	logger              *zap.Logger
	sshSigners          []ssh.Signer
	hostKeyCallback     ssh.HostKeyCallback
	moduleContext       context.Context
	moduleContextCancel context.CancelFunc
	inflightTotalReqs   atomic.Int64
	inflightLastReqs    atomic.Int64 // in seconds
	sshStatus           atomic.Int32 // 0 => unset/disconnected, 1 => configuring, 2 => connected
	sshLooperStopped    chan struct{}
}

func (s HTTPWakeOnConnect) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID: "http.reverse_proxy.transport.http_wake_on_connect",
		New: func() caddy.Module {
			return new(HTTPWakeOnConnect)
		},
	}
}

func (s *HTTPWakeOnConnect) Provision(ctx caddy.Context) error {
	var err error

	s.id = uuid.NewString()
	s.logger = ctx.Logger(s).With(zap.String("id", s.id))
	s.logger.Info("HTTPWakeOnConnect provisioning started")

	s.moduleContext, s.moduleContextCancel = context.WithCancel(context.Background())

	if s.WOCSSHTimeout <= 0 {
		s.WOCSSHTimeout = caddy.Duration(30 * time.Second)
	}
	if s.WOCSSHPollingInterval <= 0 {
		s.WOCSSHPollingInterval = caddy.Duration(250 * time.Millisecond)
	}
	if s.WOCSSHUser == "" {
		return fmt.Errorf("SSH user is required")
	}
	if s.WOCSSHAddress == "" {
		// todo validate address
		return fmt.Errorf("SSH address is required")
	}
	if s.WOCSSHCommand == "" {
		s.WOCSSHCommand = "tail -f /dev/null"
	}

	{
		// configuring sshSigners

		appendSSHSigners := func(filePaths []string) error {
			var (
				err  error
				file []byte
				key  ssh.Signer
			)

			homeDir, _ := os.UserHomeDir()

			for _, filePath := range filePaths {
				filePath = strings.TrimSpace(filePath)
				if filePath == "" {
					continue
				}
				if strings.HasPrefix(filePath, "~/") {
					filePath = filepath.Join(homeDir, filePath[2:])
				}
				file, err = os.ReadFile(filePath)
				if err != nil {
					return err
				}
				key, err = ssh.ParsePrivateKey(file)
				if err != nil {
					return err
				}
				s.sshSigners = append(s.sshSigners, key)
			}
			return nil
		}

		err = appendSSHSigners(strings.Split(s.WOCSSHKeyFiles, ","))
		if err != nil {
			return err
		}
		if len(s.sshSigners) == 0 {
			err = appendSSHSigners(defaultKeyPaths)
			if err != nil {
				return err
			}
		}
		if len(s.sshSigners) == 0 {
			return fmt.Errorf("no ssh key configured")
		}
	}

	{
		// configuring hostKeyCallback

		var paths []string
		homeDir, _ := os.UserHomeDir()

		for filePath := range strings.SplitSeq(s.WOCSSHKnownHostsFiles, ",") {
			filePath = strings.TrimSpace(filePath)
			if filePath == "" {
				continue
			}
			if strings.HasPrefix(filePath, "~/") {
				filePath = filepath.Join(homeDir, filePath[2:])
			}
			paths = append(paths, filePath)
		}
		if len(paths) == 0 {
			paths = append(paths, filepath.Join(homeDir, ".ssh", "known_hosts"))
		}
		hostKeyCallback, err := knownhosts.New(paths...)
		if err != nil {
			return err
		}
		s.hostKeyCallback = hostKeyCallback
	}

	err = s.blockingSSHCall(ctx.Context, true, nil)
	if err != nil {
		return err
	}

	s.sshLooperStopped = make(chan struct{})
	go s.looperSSHCall()

	err = s.HTTPTransport.Provision(ctx)
	if err != nil {
		return err
	}
	return nil
}

func (s HTTPWakeOnConnect) Cleanup() error {
	s.logger.Info("HTTPWakeOnConnect cleanup started")

	s.moduleContextCancel()

	err := s.HTTPTransport.Cleanup()
	if err != nil {
		return err
	}

	if s.sshLooperStopped != nil {
		<-s.sshLooperStopped // todo handle force close
	}

	return nil
}

func (s *HTTPWakeOnConnect) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
	if !d.Next() {
		return d.ArgErr()
	}

	for d.NextBlock(0) {
		switch d.Val() {
		case "woc_ssh_address":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.WOCSSHAddress = d.Val()
			d.Delete()
			d.Delete()
		case "woc_ssh_user":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.WOCSSHUser = d.Val()
			d.Delete()
			d.Delete()
		case "woc_ssh_timeout":
			if !d.NextArg() {
				return d.ArgErr()
			}
			duration, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("bad duration value '%s': %v", d.Val(), err)
			}
			s.WOCSSHTimeout = caddy.Duration(duration)
			d.Delete()
			d.Delete()
		case "woc_ssh_polling_interval":
			if !d.NextArg() {
				return d.ArgErr()
			}
			duration, err := caddy.ParseDuration(d.Val())
			if err != nil {
				return d.Errf("bad duration value '%s': %v", d.Val(), err)
			}
			s.WOCSSHPollingInterval = caddy.Duration(duration)
			d.Delete()
			d.Delete()
		case "woc_ssh_key_files":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.WOCSSHKeyFiles = d.Val()
			d.Delete()
			d.Delete()
		case "woc_ssh_known_hosts_files":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.WOCSSHKnownHostsFiles = d.Val()
			d.Delete()
			d.Delete()
		case "woc_ssh_command":
			if !d.NextArg() {
				return d.ArgErr()
			}
			s.WOCSSHCommand = d.Val()
			d.Delete()
			d.Delete()
		}
	}

	d.Reset()
	err := s.HTTPTransport.UnmarshalCaddyfile(d)
	if err != nil {
		return err
	}
	return nil
}

func (s *HTTPWakeOnConnect) blockingSSHCall(ctx context.Context, dialCheck bool, onConnect func()) error {
	s.logger.Info("starting ssh session")
	defer s.logger.Info("ssh session stopped")

	sshClient, err := ssh.Dial("tcp", s.WOCSSHAddress, &ssh.ClientConfig{
		User:            s.WOCSSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(s.sshSigners...)},
		HostKeyCallback: s.hostKeyCallback,
		Timeout:         time.Duration(s.WOCSSHTimeout),
	})
	if err != nil {
		s.logger.Error("unable to dial SSH address", zap.Error(err))
		return err
	}
	defer sshClient.Close()

	session, err := sshClient.NewSession()
	if err != nil {
		s.logger.Error("unable to make a session", zap.Error(err))
		return err
	}
	defer session.Close()

	err = session.RequestPty("xterm", 80, 40, ssh.TerminalModes{ssh.ECHO: 0})
	if err != nil {
		s.logger.Error("unable to RequestPty", zap.Error(err))
		return err
	}

	if onConnect != nil {
		onConnect()
	}
	if dialCheck {
		return nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg := &sync.WaitGroup{}
	defer wg.Wait()

	wg.Go(func() {
		<-ctx.Done()
		s.logger.Info("closing ssh session")
		_ = session.Close()
	})

	err = session.Run(s.WOCSSHCommand)
	if err != nil {
		if ctx.Err() == nil {
			// is session closed
			s.logger.Error("unable to run command", zap.Error(err))
			cancel()
			return err
		}
	}

	return nil
}

func (s *HTTPWakeOnConnect) looperSSHCall() {
	defer close(s.sshLooperStopped)

	wg := &sync.WaitGroup{}
	defer wg.Wait()

	ctxChan := make(chan context.Context)

	wg.Go(func() {
		for {
			select {
			case ctx := <-ctxChan:
				s.sshStatus.Store(1)
				id := uuid.NewString()
				logger := s.logger.With(zap.String("ssh_connection_id", id))

				var err error
			loop:
				for i := range 3 {
					logger.Debug("blocking ssh session: connecting", zap.Int("attempt", i+1))
					err = s.blockingSSHCall(ctx, false, func() {
						s.sshStatus.Store(2)
						logger.Debug("blocking ssh session: connected", zap.Int("attempt", i+1))
					})
					if err != nil {
						logger.Error("blocking ssh session: got error", zap.Int("attempt", i+1), zap.Error(err))
						continue
					}
					if err == nil {
						break loop
					}
				}
				logger.Debug("blocking ssh session: disconnected")
				s.sshStatus.Store(0)
			case <-s.moduleContext.Done():
				return
			}
		}
	})

	wg.Go(func() {
		var (
			currentContextCancel context.CancelFunc
		)

		defer func() {
			if currentContextCancel != nil {
				currentContextCancel()
			}
		}()

		for {
			if s.inflightTotalReqs.Load() > 0 {
				if s.sshStatus.Load() == 0 {
					if currentContextCancel != nil {
						currentContextCancel()
					}
					var currentContext context.Context
					currentContext, currentContextCancel = context.WithCancel(context.Background())
					ctxChan <- currentContext
				}

			} else {
				if time.Duration(time.Now().Unix()-s.inflightLastReqs.Load()).Abs()*time.Second > 30*time.Second {
					if currentContextCancel != nil {
						currentContextCancel()
						currentContextCancel = nil
					}
				}
			}

			select {
			case <-time.After(time.Duration(s.WOCSSHPollingInterval)):
			case <-s.moduleContext.Done():
				return
			}
		}
	})

	<-s.moduleContext.Done()
}

func (s *HTTPWakeOnConnect) trigger(ctx context.Context) (func(), error) {
	s.inflightTotalReqs.Add(1)

loop:
	for {
		if s.sshStatus.Load() == 2 {
			break loop
		}

		select {
		case <-time.After(time.Duration(s.WOCSSHPollingInterval)):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return func() {
		s.inflightTotalReqs.Add(-1)
		s.inflightLastReqs.Store(time.Now().Unix())
	}, nil
}

func (s *HTTPWakeOnConnect) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()

	s.logger.Debug("HTTPWakeOnConnect REQ")

	finish, err := s.trigger(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()

	res, err := s.HTTPTransport.RoundTrip(request)
	if err != nil {
		return nil, err
	}

	s.logger.Debug("HTTPWakeOnConnect RESP")

	return res, nil
}

// Interface guards: compile-time checks that you implement what you think you do
var (
	_ caddy.Module                                     = (*HTTPWakeOnConnect)(nil)
	_ caddy.Provisioner                                = (*HTTPWakeOnConnect)(nil)
	_ caddy.CleanerUpper                               = (*HTTPWakeOnConnect)(nil)
	_ caddyfile.Unmarshaler                            = (*HTTPWakeOnConnect)(nil)
	_ http.RoundTripper                                = (*HTTPWakeOnConnect)(nil)
	_ reverseproxy.TLSTransport                        = (*HTTPWakeOnConnect)(nil)
	_ reverseproxy.H2CTransport                        = (*HTTPWakeOnConnect)(nil)
	_ reverseproxy.HealthCheckSchemeOverriderTransport = (*HTTPWakeOnConnect)(nil)
	_ reverseproxy.ProxyProtocolTransport              = (*HTTPWakeOnConnect)(nil)
)
