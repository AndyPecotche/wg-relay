# Ejemplo: Caddy como terminador TLS

Expone servicios web **y MQTT nativo sobre TLS** a través de wg-relay, con
certificados de Let's Encrypt obtenidos y renovados automáticamente, sin
certbot y sin tocar tus servicios.

wg-relay nunca ve tu tráfico en claro: el TLS lo termina Caddy, en tu servidor.

## Por qué Caddy y no nginx

Caddy pide, renueva y recarga los certificados solo. Con nginx habría que
sumar certbot, un certificado autofirmado para el arranque y un recargador.
El plugin [`caddy-l4`](https://github.com/mholt/caddy-l4) agrega lo que le
falta: terminar TLS para protocolos que no son HTTP, como MQTT.

## Cómo funciona

```
          ┌──── internet ────┐
          ▼                  ▼
   app.<dominio>       mqtt.<dominio>
          │                  │
          └────── :443 ──────┘
                   │  nodo de wg-relay: rutea por SNI, no descifra
                   ▼
              agente (wgrelay.yml: todo → caddy:443)
                   ▼
          ┌─ Caddy :443 (layer4) ─────────────────────┐
          │  ¿ALPN acme-tls/1? → servidor web :8443   │  ← challenges de LE
          │  ¿SNI mqtt.*?      → termina TLS → :1883  │  ← MQTT nativo
          │  resto             → servidor web :8443   │  ← HTTP normal
          └───────────────────────────────────────────┘
```

Dos detalles que no son obvios:

1. **layer4 se queda con el `:443`,** y el servidor web de Caddy se corre al
   `8443` interno. Es necesario porque hay que mirar cada conexión antes de
   decidir si es HTTP o MQTT.
2. **La primera regla desvía los challenges de Let's Encrypt.** Sin ella,
   `mqtt.<dominio>` no podría sacar certificado nunca: el challenge llega con
   ese mismo SNI y lo comería el terminador MQTT. Por eso se mira el ALPN
   `acme-tls/1` antes que el SNI.

También por eso el `Caddyfile` usa `disable_http_challenge`: el `:80` del nodo
solo redirige a HTTPS, así que HTTP-01 no es una opción confiable acá.

## Uso

```sh
cp .env.example .env     # completá WGRELAY_TOKEN y ACME_EMAIL
docker compose up wgrelay
```

El agente imprime el dominio que te asignaron. Copialo a `WGRELAY_DOMAIN` en
`.env`, cortá con Ctrl-C y levantá todo:

```sh
docker compose --profile demo up -d --build     # con los servicios de ejemplo
docker compose logs -f caddy
```

Probá `https://app.<tu-dominio>` en el navegador, y el broker con:

```sh
mosquitto_sub -h mqtt.<tu-dominio> -p 443 --capath /etc/ssl/certs -t '#' -v
```

> Mientras probás, descomentá `ACME_CA` en `.env` para usar la CA de staging
> de Let's Encrypt y no gastar cuota. Los certificados no serán confiables
> (el navegador advierte; `mosquitto_sub` necesita `--insecure`). Al pasar a
> producción, comentá esa línea y borrá el volumen: `docker compose down -v`.

## Adaptarlo a tu stack

Sacá el perfil `demo` y apuntá Caddy a tus servicios. Para SensorHub:

```caddyfile
emqx.{$WGRELAY_DOMAIN}   { reverse_proxy emqx:18083 }
influx.{$WGRELAY_DOMAIN} { reverse_proxy influxdb:8086 }
grafana.{$WGRELAY_DOMAIN} { reverse_proxy grafana:3000 }

mqtt.{$WGRELAY_DOMAIN}   { respond "Solo MQTT sobre TLS." 421 }
```

y en el bloque `layer4`, `proxy broker:1883` → `proxy emqx:1883`.

Agregá el compose a la red de tu proyecto para que Caddy alcance esos nombres:

```yaml
networks:
  default:
    name: sensorhub_network
    external: true
```

No hace falta tocar `wgrelay.yml` al sumar subdominios: la ruta comodín (`*`)
manda todo a Caddy, y Caddy decide. Solo se toca si querés que un hostname
vaya a **otro puerto** en vez de a Caddy.

## Qué se probó

Verificado de punta a punta contra un relay real (nodo + agente + túnel):

- HTTP a través de Caddy, con la IP real del visitante llegando al backend en
  `X-Forwarded-For` vía PROXY protocol.
- MQTT nativo sobre TLS: handshake y `CONNACK` del broker.
- Una conexión con ALPN `acme-tls/1` se desvía al servidor web en vez de al
  terminador MQTT, que es el mecanismo del punto 2 de arriba.

**No probado:** la emisión real contra Let's Encrypt, que necesita un dominio
público. Las pruebas usaron la CA interna de Caddy. Si algo va a fallar la
primera vez, es acá: mirá `docker compose logs caddy`.

## Esto se simplifica pronto

Cuando esté el modo `terminate` (F1), los servicios HTTP no van a necesitar
Caddy: se declaran en `wgrelay.yml` y el propio agente termina el TLS. Este
ejemplo va a seguir siendo útil para MQTT nativo y para quien quiera control
fino sobre el proxy.
