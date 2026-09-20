FROM caddy:builder AS builder

COPY . /caddy-woc/

RUN xcaddy build \
    --with github.com/anantadwi13/caddy-wake-on-connect=/caddy-woc

FROM caddy:latest

COPY --from=builder /usr/bin/caddy /usr/bin/caddy