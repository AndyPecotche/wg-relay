# Runbook: activar el DNS propio (self-hosted) en el VPS y probarlo de punta a punta

Objetivo: actualizar el VPS real con el código de la rama `delegacion-ns`
(DNS autoritativo propio para `clients.*`, sin depender de Cloudflare para
eso), y probarlo de verdad exponiendo un servicio corrido en la laptop
conectada por datos móviles.

Reemplazá estos placeholders por tus valores reales en todo el documento:

| Placeholder | Qué es | Dónde lo sacás |
|---|---|---|
| `andy.net.ar` | tu dominio | — |
| `wg-relay.andy.net.ar` | `WGRELAY_EDGE_HOST` | tu `.env` actual del servidor |
| `clients.wg-relay.andy.net.ar` | `WGRELAY_BASE_DOMAIN` | tu `.env` actual del servidor |
| `VPS_IP` | IP pública de tu VPS | `curl -4 ifconfig.me` en el VPS |
| `~/wg-relay` | ruta del repo clonado en el VPS | donde lo hayas clonado |

## ⚠️ Antes de arrancar: entendé el riesgo

Este VPS ya está sirviendo tunnels reales. Delegar `clients.*` por NS en
Cloudflare **no es aditivo**: en el momento en que agregás esos registros NS,
Cloudflare deja de responder por **todo** lo que cuelgue de `clients.*` —
incluidos los CNAME de los tunnels que ya existen — y pasa a responder
tu propio nodo. Si el nodo tuviera un problema en ese momento, los tunnels
existentes quedarían sin resolver DNS (el tráfico ya establecido no se
corta, pero nadie nuevo podría resolver el hostname).

Por eso el orden de este runbook **no** es "Cloudflare primero": es al
revés. Primero armás y validás todo del lado del VPS, consultando el
servidor DNS **directamente** (sin pasar por Cloudflare, sin tocar nada
público). Recién cuando eso funciona 100%, tocás Cloudflare — y ese paso
queda como el último, el más corto, y el único con riesgo real.

Antes de tocar nada:

```sh
# En el VPS, backup de la base antes de cualquier migración.
docker compose exec postgres pg_dump -U wgrelay wgrelay > ~/wgrelay-backup-$(date +%Y%m%d).sql
```

Si en cualquier momento después de delegar algo se ve mal: borrá (o
comentá) los dos registros NS en Cloudflare y volvés al estado anterior —
no hace falta revertir nada del lado del VPS, `WGRELAY_DNS_NS_NAMES` puede
quedar configurado sin problema aunque la delegación externa no exista (el
nodo simplemente sigue contestando igual, nadie externo le pregunta).

---

## Parte 1 — Actualizar el código en el VPS

```sh
# 1. Conectate al VPS y andá al repo.
ssh tu-vps
cd ~/wg-relay

# 2. Traé la rama nueva (todavía no está mergeada a main).
git fetch origin
git checkout delegacion-ns
git pull origin delegacion-ns

# 3. Confirmá que estás donde pensás.
git log --oneline -1     # debería mostrar el commit "delegacionns"
```

No hace falta build aparte: `deploy/server/docker-compose.yml` ya tiene
`build: {context: ../.., target: api}` (y lo mismo para `node`), así que
`docker compose up -d --build` compila desde este código en el momento.

## Parte 2 — Configurar y levantar (sin tocar Cloudflare todavía)

```sh
cd ~/wg-relay/deploy/server
```

Editá `.env` y agregá (dejá lo que ya tenías: Cloudflare y DNS propio
conviven sin problema, y por ahora esto no afecta nada externo):

```sh
# Nombres de nameserver: elegí un nombre para tu nodo actual bajo tu
# EDGE_HOST (todavía no existe como registro, lo creamos en la Parte 4).
WGRELAY_DNS_NS_NAMES=ns1.wg-relay.andy.net.ar
WGRELAY_DNS_SOA_EMAIL=vos@andy.net.ar
```

En `deploy/server/docker-compose.yml`, en el servicio `node`, descomentá:

```yaml
      WGRELAY_DNS_LISTEN: ":5300"
    ports:
      - "443:8443/tcp"
      - "80:8080/tcp"
      - "51820:51820/udp"
      - "53:5300/udp"
      - "53:5300/tcp"
```

Levantá (esto reconstruye `api` y `node`, y de paso aplica las dos
migraciones nuevas — `node.public_ip` y `dns_challenge.value` — contra la
base ya existente, sin tocar los datos de los tunnels):

```sh
docker compose up -d --build
docker compose logs api --tail 20   # sin errores de migración
```

Cargale la IP pública a tu nodo actual (iba a ser NULL si lo creaste antes
de esta rama — sin esto, el nodo no entra en el set de IPs que se contesta):

```sh
docker compose exec api wgrelay-api node list
#  ← anotá el ID de tu nodo

docker compose exec api wgrelay-api node set-public-ip --id 1 --public-ip VPS_IP
```

## Parte 3 — Verificar el DNS SIN tocar nada público (riesgo cero)

Primero, local, desde dentro del propio VPS, contra el puerto interno del
contenedor:

```sh
docker compose exec node sh -c 'echo esto probablemente no funciona: no hay shell en la imagen'
```

La imagen del nodo es distroless (sin shell), así que probamos desde el
host del VPS con `dig` (instalalo si hace falta: `apt install -y
dnsutils` / `apk add bind-tools`, según la distro):

```sh
dig @127.0.0.1 -p 5300 NS clients.wg-relay.andy.net.ar
dig @127.0.0.1 -p 5300 A cualquiercosa.clients.wg-relay.andy.net.ar
dig @127.0.0.1 -p 5300 A clients.wg-relay.andy.net.ar
```

Esperado:

- El primero devuelve `ns1.wg-relay.andy.net.ar.` como NS.
- Los otros dos devuelven **la IP pública del VPS** como registro A —
  cualquier nombre bajo la zona contesta con el mismo set de nodos
  activos, no hace falta que el subdominio "exista" como tunnel.

Si algo de esto no funciona, **no sigas a la Parte 4**. Revisá
`docker compose logs node --tail 50` y `docker compose logs api --tail 50`
(que `WGRELAY_DNS_NS_NAMES` no esté vacío, que el nodo tenga `public_ip`
cargada, que el long-poll haya aplicado la config — buscá la línea
`"configuración aplicada"` en los logs del nodo).

Ahora sí, abrí el firewall del VPS para que se pueda consultar desde
afuera (todavía sin delegar nada en Cloudflare, esto solo habilita que
CUALQUIERA pueda preguntarle directo a tu nodo, como cualquier DNS
público):

```sh
# ejemplo con ufw; si tu VPS usa otra cosa (security group de la nube,
# firewalld, iptables a mano), el equivalente es "abrir 53/udp y 53/tcp"
sudo ufw allow 53/udp
sudo ufw allow 53/tcp
```

Y repetí las mismas consultas, esta vez desde tu laptop (o el celular),
apuntando directo al VPS en vez de a localhost:

```sh
dig @VPS_IP NS clients.wg-relay.andy.net.ar
dig @VPS_IP A cualquiercosa.clients.wg-relay.andy.net.ar
```

Si esto funciona, el DNS propio está 100% operativo — lo único que falta
es decirle al mundo que le pregunte a él.

## Parte 4 — Cloudflare: delegar (el único paso con riesgo, y el más corto)

En el panel de Cloudflare, zona `andy.net.ar`:

1. **Crear un registro A** (no NS todavía): `ns1.wg-relay` → `VPS_IP`.
   Proxy **desactivado** ("DNS only", nube gris) — obligatorio, si no
   Cloudflare intentaría servir tráfico HTTP/HTTPS por su propia IP en vez
   de resolver a la tuya.
2. **Crear el registro NS**: nombre `clients.wg-relay`, apunta a
   `ns1.wg-relay.andy.net.ar`. (Cloudflare va a pedirte el nombre completo
   del nameserver con el punto final o sin él según la UI del momento;
   cualquiera de los dos formatos funciona.)

Con un solo nameserver alcanza para esta prueba. Para producción de
verdad convendría un segundo nodo con IP distinta declarado como
`ns2.wg-relay.andy.net.ar`, así una caída de un solo nodo no tira abajo la
resolución — no hace falta ahora, se agrega después sin tocar nada de lo
demás (§4.3.1 de DESIGN.md).

Verificá la propagación (puede tardar de segundos a un par de minutos
según cómo cachee tu propio resolver — probar contra `8.8.8.8` o
`1.1.1.1` directamente salta la caché local):

```sh
dig NS clients.wg-relay.andy.net.ar @8.8.8.8
dig A cualquiercosa.clients.wg-relay.andy.net.ar @8.8.8.8
```

Cuando esto devuelva la IP del VPS sin tener que especificar el `@ns1...`
a mano, la delegación real está funcionando.

---

## Parte 5 — La laptop: agente + servicio de ejemplo, por datos móviles

Importante: **la laptop no necesita IP pública para nada de esto.** Es
justamente al revés — wg-relay existe para exponer servicios detrás de
NAT/CGNAT (que es exactamente lo que suele haber en datos móviles). El
agente se conecta *hacia afuera* al VPS; nadie necesita conectarse
*hacia* la laptop directamente.

Y sí, confirmado: **no hace falta compilar nada en la laptop.** El código
de DNS propio tocó solo `internal/api`, `internal/node`, `internal/store` y
`internal/proto` — el agente (`internal/agent`, `cmd/wgrelay-agent`) no
cambió un carácter en esta rama. La imagen pública de siempre sirve tal
cual:

```
ghcr.io/andypecotche/wg-relay-agent:latest
```

(Si por lo que sea `:latest` todavía no está publicada — se publica al
mergear a `main`, no a `delegacion-ns` — usá el tag de la última release
que ya tengas funcionando, o compilá una vez con `docker build --target
agent -t wgrelay-agent:dev .` desde el repo. Cualquiera de las dos te
sirve igual: el agente es idéntico.)

En la laptop, una carpeta nueva:

```sh
mkdir ~/wgrelay-demo && cd ~/wgrelay-demo
```

`docker-compose.yml`:

```yaml
services:
  whoami:
    image: traefik/whoami
    networks: [demo]

  wgrelay:
    image: ghcr.io/andypecotche/wg-relay-agent:latest
    restart: unless-stopped
    environment:
      WGRELAY_TOKEN: ${WGRELAY_TOKEN:?}
    volumes:
      - ./wgrelay.yml:/etc/wgrelay/wgrelay.yml:ro
    networks: [demo]

networks:
  demo:
```

`wgrelay.yml`:

```yaml
relay: https://api.wg-relay.andy.net.ar

acme:
  email: vos@ejemplo.com

routes:
  # terminate (default): TLS-ALPN-01, no necesita el DNS propio para el
  # certificado — deja aislado lo que estamos probando (que resuelva el
  # hostname), sin mezclarlo con DNS-01/wildcard.
  - host: demo
    to: http://whoami:80
```

`.env`:

```sh
WGRELAY_TOKEN=
```

## Parte 6 — Crear el tunnel de prueba (en el VPS)

```sh
# en el VPS
docker compose exec api wgrelay-api tunnel create --email demo@vos.com
```

Anotá el **dominio asignado** (algo como `xyzabc.clients.wg-relay.andy.net.ar`)
y el **token** (`wgr_...`, se muestra una sola vez). Pegá el token en el
`.env` de la laptop.

## Parte 7 — Levantar el agente en la laptop (conectada a datos móviles)

```sh
cd ~/wgrelay-demo
docker compose up -d
docker compose logs -f wgrelay
```

Esperá el mensaje de "túnel establecido". El agente no necesita ningún
puerto abierto ni configuración de red especial — sale por datos móviles
como cualquier cliente HTTP normal.

## Parte 8 — Verificación real, desde cualquier lado de internet

Desde el celular (con wifi, no datos móviles, para no pegarle a la misma
red), o pidiéndole a alguien:

```sh
curl https://xyzabc.clients.wg-relay.andy.net.ar/demo
```

(ajustá el path si hace falta; `whoami` responde en `/` también). Tendría
que devolver la respuesta de `whoami`, con la IP de origen siendo la del
nodo del VPS (PROXY protocol) y **sin ningún error de certificado** — el
agente sacó su propio certificado por Let's Encrypt, sin que vos tocaras
nada de eso.

Y para chequear específicamente que fue el DNS propio el que resolvió
(no una caché vieja de Cloudflare):

```sh
dig A xyzabc.clients.wg-relay.andy.net.ar @ns1.wg-relay.andy.net.ar
```

Si devuelve la IP del VPS consultando exactamente a tu propio nameserver,
la prueba está completa: creaste un tunnel nuevo sin que el control plane
llamara a ninguna API externa, y resolvió en el DNS público real.

---

## Troubleshooting rápido

| Síntoma | Dónde mirar |
|---|---|
| `dig @127.0.0.1 -p 5300` no contesta nada | `docker compose logs node`: ¿arrancó el listener? ¿`WGRELAY_DNS_LISTEN` quedó comentado? |
| Contesta localhost pero no desde afuera | Firewall del VPS (¿`53/udp` Y `53/tcp` abiertos? Pebble/ACME real también puede necesitar TCP) |
| `NS` no trae nada / trae vacío | `WGRELAY_DNS_NS_NAMES` vacío en el `.env` de la API, o la API no se reinició después de setearlo |
| `A` de un subdominio no trae nada | `node list` → ¿el nodo tiene `public_ip` cargada? ¿está "activo" (heartbeat reciente)? |
| Cloudflare no deja crear el NS | Asegurate de que el registro A de `ns1.wg-relay` ya exista primero (algunos paneles validan que el target resuelva) |
| Después de delegar, tunnels viejos dejan de resolver | Volvé a dig directo contra `ns1...`: si eso funciona pero el público no, es propagación/caché, esperá unos minutos; si tampoco funciona directo, borrá el NS en Cloudflare y revisá logs del nodo |
