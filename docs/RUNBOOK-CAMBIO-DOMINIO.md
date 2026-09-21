# Runbook: cambiar el dominio del servicio (corte directo)

Objetivo: migrar wg-relay de un dominio a otro (ejemplo acá:
`andy.net.ar` → `wgr.com.ar`) en un VPS que ya está en producción, con
tunnels reales activos.

Reemplazá estos placeholders por tus valores reales en todo el documento:

| Placeholder | Qué es |
|---|---|
| `andy.net.ar` | tu dominio actual |
| `wgr.com.ar` | tu dominio nuevo |
| `wg-relay` | el subdominio que uses como base en ambos (podés cambiarlo también, no hace falta) |
| `VPS_IP` | IP pública de tu VPS |
| `~/wg-relay` | ruta del repo clonado en el VPS |

## ⚠️ Antes de arrancar: entendé el riesgo (es corte directo, no gradual)

Elegiste corte directo, así que es importante tener claro qué se rompe en
el momento exacto en que reiniciás con el `.env` nuevo — **todo pasa al
dominio nuevo a la vez, no hay convivencia**:

1. **Los agentes ya desplegados dejan de conectarse.** Cada cliente tiene
   `relay: https://api.wg-relay.andy.net.ar` fijo en su propio
   `wgrelay.yml` (un archivo que vive en infraestructura de cada uno, no en
   este repo). La API solo puede servir **un dominio a la vez**
   (`WGRELAY_API_DOMAIN` es un string único, no una lista — a diferencia de
   `WGRELAY_DNS_NS_NAMES`, que sí es CSV). Apenas reiniciás, el hostname
   viejo de la API deja de responder — cada agente necesita que le
   actualicen ese archivo y lo redeployen (Parte 6).
2. **Los subdominios de tunnels existentes bajo el dominio viejo dejan de
   resolver.** El dominio de cada tunnel no se guarda como texto — se arma
   en el momento (`subdomain + WGRELAY_BASE_DOMAIN` actual). Al cambiar
   `WGRELAY_BASE_DOMAIN`, tu propio nodo (el mismo que hoy es autoritativo
   para `clients.wg-relay.andy.net.ar` por la delegación NS que ya armaste)
   pasa a contestar solo la zona nueva — cualquier consulta a la zona vieja
   le queda `REFUSED`. El `subdomain` random (ej. `dsk7yrh`) no cambia, solo
   el sufijo del dominio.

Ninguno de los dos puntos es gradual con la config actual: pasan en el
mismo instante que reiniciás. Si preferís una migración sin corte (los dos
dominios funcionando en paralelo un tiempo), hace falta un cambio de código
antes (que `WGRELAY_API_DOMAIN` acepte una lista, como ya hace
`WGRELAY_DNS_NS_NAMES`) — avisame si en algún momento preferís ese camino
en vez de este runbook.

Antes de tocar nada:

```sh
# En el VPS, backup de la base antes de cualquier cambio.
docker compose exec postgres pg_dump -U wgrelay wgrelay > ~/wgrelay-backup-$(date +%Y%m%d).sql
```

Es reversible: si algo sale mal, volvés el `.env` a los valores viejos y
reiniciás — no hace falta tocar la base (Parte 8, Rollback).

---

## Parte 1 — Anotar quién depende del dominio viejo

Antes de cortar nada, tené a mano la lista de tunnels activos y sus
contactos, para poder avisarles en la Parte 6:

```sh
cd ~/wg-relay/deploy/server
docker compose exec api wgrelay-api tunnel list
```

---

## Parte 2 — DNS del dominio nuevo (sin tocar el viejo todavía)

Mismo procedimiento que ya hiciste para `clients.*` de `andy.net.ar`, pero
ahora completo (acá sí hacen falta los registros manuales de siempre,
además de la delegación):

En tu proveedor de DNS para `wgr.com.ar` (Cloudflare u otro — si `wgr.com.ar`
todavía no tiene un proveedor elegido, ver la conversación sobre
"delegación" vs "autodelegación" en NIC.ar: delegá a un proveedor externo
para el dominio completo, **no** autodelegación directa a tu nodo):

- [ ] `api.wg-relay` → **A** → `VPS_IP` (DNS only, sin proxy)
- [ ] `wg-relay` → **A** → `VPS_IP` (edge)
- [ ] `node1.wg-relay` → **A** → `VPS_IP` (endpoint WireGuard de cada nodo
      que vayas a migrar — ver Parte 4)
- [ ] Registro **A** para el nameserver propio: `ns1.wg-relay` → `VPS_IP`
      (sin proxy)
- [ ] Registro **NS**: `clients.wg-relay` → `ns1.wg-relay.wgr.com.ar`

Verificá que **resuelva** (todavía no podés probar que conteste bien —
la API sigue sirviendo el dominio viejo hasta la Parte 5 — pero sí que el
DNS esté propagado, que es requisito para que el certificado de la API
se pueda emitir en la Parte 5):

```sh
dig +short api.wg-relay.wgr.com.ar     # → VPS_IP
dig +short wg-relay.wgr.com.ar         # → VPS_IP
dig NS clients.wg-relay.wgr.com.ar @8.8.8.8   # → ns1.wg-relay.wgr.com.ar
```

Si algo de esto no resuelve todavía, esperá la propagación antes de seguir
a la Parte 3.

---

## Parte 3 — Actualizar `.env` en el VPS

```sh
cd ~/wg-relay/deploy/server
nano .env
```

Cambiá estas cinco variables (dejá el resto igual):

```diff
- WGRELAY_API_DOMAIN=api.wg-relay.andy.net.ar
+ WGRELAY_API_DOMAIN=api.wg-relay.wgr.com.ar

- WGRELAY_EDGE_HOST=wg-relay.andy.net.ar
+ WGRELAY_EDGE_HOST=wg-relay.wgr.com.ar

- WGRELAY_BASE_DOMAIN=clients.wg-relay.andy.net.ar
+ WGRELAY_BASE_DOMAIN=clients.wg-relay.wgr.com.ar

- WGRELAY_DNS_NS_NAMES=ns1.wg-relay.andy.net.ar
+ WGRELAY_DNS_NS_NAMES=ns1.wg-relay.wgr.com.ar

- WGRELAY_DNS_SOA_EMAIL=vos@andy.net.ar
+ WGRELAY_DNS_SOA_EMAIL=vos@wgr.com.ar
```

---

## Parte 4 — Migrar el endpoint WireGuard de cada nodo

`node.endpoint` (el `nodeN.wg-relay.andy.net.ar:51820` que ves en
`node list`) es un valor guardado, no derivado de `WGRELAY_BASE_DOMAIN` ni
de ningún otro env var. Ahora hay un comando dedicado para actualizarlo sin
recrear el nodo (recrearlo sería disruptivo: un nodo nuevo implica token
nuevo y reasignación de `gateway_ip`):

```sh
docker compose exec api wgrelay-api node list
#  ← anotá el ID de cada nodo

docker compose exec api wgrelay-api node set-endpoint --id 1 --endpoint node1.wg-relay.wgr.com.ar:51820
```

Repetí por cada nodo que tengas. El cambio queda guardado en la base — los
agentes lo toman solo en la **próxima** sincronización/reconexión, no hace
falta reiniciarlos a mano por esto (si querés forzarlo ya, `docker compose
restart wgrelay` del lado de cada agente).

Si preferís no migrar un nodo puntual todavía (por ejemplo porque perdiste
el dominio viejo para uno pero no para otro), simplemente no corras el
comando para ese nodo — sigue funcionando con su endpoint actual sin que
nada más se rompa.

---

## Parte 5 — Reiniciar y verificar

```sh
cd ~/wg-relay/deploy/server
docker compose up -d
docker compose logs api --tail 30
docker compose logs node --tail 30
```

Esperado en los logs: la API obtiene un certificado nuevo por TLS-ALPN-01
para `api.wg-relay.wgr.com.ar` (buscá `"certificate obtained successfully"`
en `docker compose logs api`). Si no aparece o hay errores de challenge,
**no sigas a la Parte 6** — revisá que el DNS de la Parte 2 esté realmente
propagado (`dig +short api.wg-relay.wgr.com.ar` desde afuera del VPS
también, no solo desde adentro).

```sh
curl https://api.wg-relay.wgr.com.ar/healthz              # → ok
docker compose exec api wgrelay-api node list              # nodos, "0s atrás"
docker compose exec api wgrelay-api tunnel list             # dominios ahora con sufijo wgr.com.ar
```

Y el DNS propio de la zona nueva, mismo chequeo que ya conocés:

```sh
dig @VPS_IP NS clients.wg-relay.wgr.com.ar
dig @VPS_IP A cualquiercosa.clients.wg-relay.wgr.com.ar   # → VPS_IP
```

---

## Parte 6 — Migrar los agentes ya desplegados

Para cada tunnel de la lista que sacaste en la Parte 1: quien tenga ese
agente corriendo necesita cambiar una sola línea de su `wgrelay.yml` y
redeployar:

```diff
- relay: https://api.wg-relay.andy.net.ar
+ relay: https://api.wg-relay.wgr.com.ar
```

```sh
docker compose up -d --force-recreate wgrelay   # o el nombre del servicio del agente
docker compose logs -f wgrelay                   # esperar "túnel establecido"
```

El resto del `wgrelay.yml` (rutas, `host:`, `to:`) no cambia — el dominio
completo lo arma el agente con lo que le devuelve la API en el momento,
nunca está hardcodeado ahí.

Mientras algún agente no haya migrado, va a aparecer en los logs de la API
intentando conectar contra el dominio viejo y fallando — es esperable, no
es un error del servidor.

---

## Parte 7 — Limpieza (opcional, cuando ya migraron todos)

- [ ] Borrar (o dejar, no molesta) los registros DNS del dominio viejo,
      **excepto** el de `node1.wg-relay.andy.net.ar` si seguís usándolo
      como endpoint WireGuard (Parte 4).
- [ ] Si el dominio viejo tenía un proveedor externo configurado
      (`CLOUDFLARE_API_TOKEN`/`CLOUDFLARE_ZONE_ID` en el `.env` viejo) y ya
      no lo necesitás para nada, podés sacarlo del `.env`.

---

## Parte 8 — Rollback

Si algo sale mal en la Parte 5 (la API no consigue el certificado, o algo
no resuelve bien) y necesitás volver atrás ya:

```sh
cd ~/wg-relay/deploy/server
nano .env    # volver las 5 variables a los valores de andy.net.ar
docker compose up -d
```

Como el dominio de los tunnels se computa en el momento (no se guarda),
revertir el `.env` alcanza para que todo vuelva a responder bajo el
dominio viejo — no hace falta restaurar el backup de Postgres salvo que
algo más, ajeno a este cambio, se haya roto.

---

## Troubleshooting rápido

| Síntoma | Dónde mirar |
|---|---|
| La API no consigue el certificado nuevo | `docker compose logs api`: ¿el DNS de `api.wg-relay.wgr.com.ar` estaba realmente propagado antes del restart? `dig` desde afuera del VPS, no solo `127.0.0.1` |
| `curl https://api.wg-relay.wgr.com.ar/healthz` no contesta | ¿`WGRELAY_LOCAL_ROUTES` en el nodo sigue templado con `${WGRELAY_API_DOMAIN}`? (`deploy/server/docker-compose.yml`, no debería hacer falta tocarlo, pero confirmá que no se haya hardcodeado en algún momento) |
| Un agente no reconecta después de migrar su `wgrelay.yml` | `docker compose logs` del agente: ¿el `relay:` quedó bien escrito? ¿el token sigue siendo el mismo (no hace falta uno nuevo, solo cambia el dominio)? |
| `dig @VPS_IP A ... clients.wg-relay.wgr.com.ar` no contesta | `docker compose logs node`: ¿aplicó la config nueva? Buscá `"configuración aplicada"` |
| Tunnels bajo el dominio viejo siguen resolviendo cuando no deberían | Puede ser caché de un resolver intermedio — probá contra `ns1.wg-relay.andy.net.ar` directo, no contra `8.8.8.8`/`1.1.1.1` |
