# Caddy con el plugin layer4, que permite terminar TLS para protocolos que no
# son HTTP (MQTT, Postgres, lo que sea) usando los mismos certificados.
FROM caddy:2-builder AS builder
RUN xcaddy build --with github.com/mholt/caddy-l4

FROM caddy:2
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
