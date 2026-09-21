# wg-relay

Exponé servicios que corren detrás de NAT o CGNAT a través de un relay con IP
pública, como ngrok o Cloudflare Tunnel, pero autohospedado y **sin que el
relay vea tu tráfico**: el TLS llega intacto a tu servidor.

- Ruteo por SNI en el puerto 443: HTTPS, MQTTS, Postgres/TLS, gRPC…
- Túnel WireGuard en espacio de usuario: el agente no necesita root ni
  privilegios.
- Certificados automáticos: el agente los obtiene y renueva solo, y los guarda
  cifrados en el servidor con una clave que el servicio no conoce.
- Del lado del cliente: un token y un archivo de configuración. Nada más.

## Usarlo (cliente)

```yaml
# wgrelay.yml — se puede commitear, no tiene secretos
relay: https://api.wg-relay.wgr.com.ar
acme:
  email: vos@ejemplo.com
routes:
  - host: influx          # → https://influx.<tu-dominio-asignado>
    to: http://influxdb:8086      # el agente saca el certificado solo

  - host: mqtt            # TLS intacto hasta tu servicio
    mode: passthrough
    to: emqx:8883
```

```sh
echo "WGRELAY_TOKEN=wgr_..." > .env      # no commitear
docker compose up -d                     # ver deploy/client/
```

```
  Dominio:  o67fzaw.clients.wg-relay.wgr.com.ar

  influx.o67fzaw.clients.wg-relay.wgr.com.ar:443  →  http://influxdb:8086  (terminate)
  mqtt.o67fzaw.clients.wg-relay.wgr.com.ar:443    →  emqx:8883  (passthrough)

level=INFO msg="túnel establecido" nodo=node1
level=INFO msg="terminando TLS" hosts=influx.o67fzaw.clients.wg-relay.wgr.com.ar
```

## Hospedarlo

Una VM con Docker y una IPv4 pública: [docs/DEPLOY.md](docs/DEPLOY.md).

## Documentación

- [docs/CASOS-DE-USO.md](docs/CASOS-DE-USO.md) — qué modo conviene en cada
  situación, con ejemplos completos, y qué falta cubrir.
- [docs/DESIGN.md](docs/DESIGN.md) — cómo funciona por dentro y por qué.
- [docs/DEPLOY.md](docs/DEPLOY.md) — levantar el servicio.

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
