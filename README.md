# wg-relay

Exponé servicios que corren detrás de NAT o CGNAT a través de un relay con IP
pública, como ngrok o Cloudflare Tunnel, pero autohospedado y **sin que el
relay vea tu tráfico**: el TLS llega intacto a tu servidor.

- Ruteo por SNI en el puerto 443: HTTPS, MQTTS, Postgres/TLS, gRPC…
- Túnel WireGuard en espacio de usuario: el agente no necesita root ni
  privilegios.
- Del lado del cliente: un token y un archivo de configuración. Nada más.

## Usarlo (cliente)

```yaml
# wgrelay.yml — se puede commitear, no tiene secretos
relay: https://api.wg-relay.andy.net.ar
routes:
  - host: mqtt            # → mqtt.<tu-dominio-asignado>
    mode: passthrough
    to: emqx:8883
```

```sh
echo "WGRELAY_TOKEN=wgr_..." > .env      # no commitear
docker compose up -d                     # ver deploy/client/
```

```
  Dominio:  o67fzaw.clients.wg-relay.andy.net.ar
  mqtt.o67fzaw.clients.wg-relay.andy.net.ar:443  →  emqx:8883  (passthrough)

level=INFO msg="túnel establecido" nodo=node1
```

## Hospedarlo

Una VM con Docker y una IPv4 pública: [docs/DEPLOY.md](docs/DEPLOY.md).

## Cómo funciona

[docs/DESIGN.md](docs/DESIGN.md).

```
cmd/wgrelay-api     control plane + CLI de administración
cmd/wgrelay-node    relay público: WireGuard + router SNI
cmd/wgrelay-agent   cliente
internal/           paquetes compartidos
deploy/             docker compose de servidor, nodo y cliente
test/e2e/           prueba de punta a punta en Docker
```

## Desarrollo

```sh
go test ./...
for t in api node agent; do docker build -q --target $t -t wgrelay-$t:dev .; done
test/e2e/run.sh
```
