# wg-relay — Documento de Diseño

> Estado: **F0 implementada** (túnel, registro, ruteo SNI, modo passthrough).
> Este documento describe el sistema completo; lo pendiente está marcado con la
> fase en la que llega. Despliegue: [DEPLOY.md](DEPLOY.md).

---

## 1. Objetivo

Exponer a internet servicios que corren en servidores sin IP pública (detrás de
NAT o CGNAT), mediante nodos relay con IPv4 pública y un túnel iniciado desde
el servidor del cliente.

Equivalente funcional a ngrok o Cloudflare Tunnel, con tres diferencias
deliberadas:

1. **Autohospedado y replicable.** Todo levanta con `docker compose` en
   cualquier VM con Docker.
2. **El relay no termina TLS.** No ve el tráfico en claro ni custodia claves
   privadas de clientes. Es un proxy L4 puro.
3. **No se limita a HTTP.** Cualquier protocolo sobre TLS se rutea por SNI
   (MQTTS, Postgres/TLS, gRPC). TCP crudo queda para más adelante, con puertos
   dedicados.

Y una restricción de experiencia de uso: **el cliente mantiene un solo archivo
de configuración (versionable) y un token.** Nada más: ni volúmenes, ni
privilegios, ni estado local.

### Fuera de alcance por ahora

- TCP sin TLS y UDP. Requieren puertos dedicados (§11.2).
- Panel web y self-service de registro. Los tokens se emiten por CLI.
- Facturación.

---

## 2. Conceptos

| Concepto | Definición |
|---|---|
| **Account** | Un usuario del servicio. Tiene un plan. |
| **Tunnel** | Un servidor del cliente. Tiene un dominio, una IP de VPN y un token. **Un token = un tunnel = un servidor.** |
| **Token** | Credencial del tunnel. Rotable sin perder el tunnel. |
| **Agent** | El binario que corre en el servidor del cliente. Sin estado persistente. |
| **Lease** | Qué instancia de agente opera un tunnel ahora mismo. Vence si no se renueva. |
| **Node** | Un relay con IP pública: termina WireGuard y rutea por SNI. |
| **Route** | Declarada en `wgrelay.yml`: `hostname → destino` con un modo. |

---

## 3. Arquitectura

```
                     ┌───────────────────────────────────────┐
                     │  wgrelay-api   (control plane)        │
                     │  · Auth por token                     │
                     │  · IPAM: IP de VPN + subdominio       │
                     │  · Leases de agentes                  │
                     │  · DNS (Cloudflare)                   │
                     │  · ACME DNS-01 delegado        [F1]   │
                     │  · Almacén cifrado de certs    [F1]   │
                     │  · Estado → PostgreSQL                │
                     └──────────────┬────────────────────────┘
                                    │ long-poll de configuración
            ┌───────────────────────┼───────────────────────┐
            ▼                       ▼                       ▼
   ┌──────────────────┐   ┌──────────────────┐   ┌──────────────────┐
   │  wgrelay-node 1  │   │  wgrelay-node 2  │   │  wgrelay-node N  │
   │  :443 SNI router │   │                  │   │                  │
   │  :80  → 308      │   │                  │   │                  │
   │  :51820 WG (udp) │   │                  │   │                  │
   └────────┬─────────┘   └──────────────────┘   └──────────────────┘
            │ WireGuard; dentro: PROXY v2 + TLS intacto
            ▼
   ┌────────────────────────────────────────────┐
   │  wgrelay-agent   (servidor del cliente)    │
   │  · WireGuard userspace, clave en memoria   │
   │  · Despacho por SNI según wgrelay.yml      │
   │  · passthrough → servicio del usuario      │
   │  · terminate   → TLS propio + HTTP   [F1]  │
   └────────────────────────────────────────────┘
```

Tres binarios Go en un solo módulo. Los tipos de la API viven en
`internal/proto`, compartidos por los tres: cliente y servidor no pueden
desincronizarse.

### 3.1 WireGuard en espacio de usuario, en ambos extremos

Nodo y agente usan `wireguard-go` con la pila TCP/IP de gVisor (`netstack`).
No se crea ninguna interfaz en el kernel. Consecuencias:

- **El agente no necesita root, `NET_ADMIN`, `/dev/net/tun` ni módulo del
  kernel.** Corre en Docker Desktop (Mac/Windows), Docker rootless, Kubernetes o
  como binario suelto. Para una imagen que va a instalar gente ajena, esto es
  decisivo.
- El nodo tampoco: su compose solo publica tres puertos.
- **No hay IP forwarding en ningún lado.** El router del nodo es un proceso que
  origina la conexión hacia el agente desde su propia IP de VPN; el agente
  acepta y abre otra conexión hacia el servicio. El aislamiento entre tenants
  es estructural: no existe ninguna ruta de un cliente a otro, ni una regla de
  iptables que alguien pueda borrar.

**Costo aceptado:** el rendimiento de netstack es menor que el de WireGuard en
el kernel (del orden de cientos de Mbps a pocos Gbps por nodo, según CPU).
Holgado para el uso previsto. Si hiciera falta, la interfaz `wgnet` permite
cambiar el nodo a WireGuard de kernel sin tocar el agente.

### 3.2 El nodo rutea a *tunnels*, el agente rutea a *servicios*

El nodo solo sabe `hostname → tunnel` (`<sub>.<base>` y `*.<sub>.<base>` para
cada tunnel en línea) y siempre conecta al puerto 443 del agente dentro del
túnel. Es el agente el que, con su `wgrelay.yml`, decide a qué servicio y en
qué modo va cada hostname.

Consecuencias:

- Agregar o cambiar rutas no toca al control plane ni a los nodos: se edita
  `wgrelay.yml` y se reinicia el agente.
- El nodo tiene una tabla chica y estable (dos entradas por tunnel en línea).
- El "wildcard" del cliente sale gratis: cualquier `x.<sub>.<base>` llega a su
  agente, y el agente decide.

### 3.3 Por qué no hay nginx ni Caddy

- **Nodo:** el router propio se reconfigura en caliente (tabla inmutable detrás
  de un `atomic.Pointer`), sin archivos ni reloads. nginx tampoco puede abrir
  puertos nuevos sin reload, bloqueante para los puertos dedicados futuros.
- **Agente [F1]:** `certmagic` (la librería ACME de Caddy) + `httputil.ReverseProxy`
  dan TLS automático en un solo binario. Se pierde el surface de config de
  Caddy (compresión, headers, basic auth); la válvula de escape es apuntar
  una ruta `passthrough` al nginx/Caddy propio del usuario.

---

## 4. Direccionamiento y DNS

### 4.1 Espacio de VPN

```
10.10.0.0/16     Gateways de nodos.  Nodo N → 10.10.N.1     (hasta 254 nodos)
10.64.0.0/10     Pool de clientes.   Un /32 único GLOBAL por tunnel (~4M)
```

La IP del cliente es única en todo el sistema, no por nodo. Por eso un agente
puede tener túnel con todos los nodos a la vez: cada nodo es un peer con
`AllowedIPs = 10.10.N.1/32`, sin solapamiento. Las IPs nunca se reutilizan.

### 4.2 Registros DNS

| Nombre | Tipo | Quién lo crea |
|---|---|---|
| `wg-relay.andy.net.ar` (*edge*) | A, uno por nodo | Admin, al sumar un nodo |
| `nodeN.wg-relay.andy.net.ar` | A | Admin, al sumar un nodo |
| `api.wg-relay.andy.net.ar` | A → VM de la API | Admin, una vez |
| `<sub>.clients.wg-relay.andy.net.ar` | CNAME → edge | API, al crear el tunnel |
| `*.<sub>.clients.wg-relay.andy.net.ar` | CNAME → edge | API, al crear el tunnel |
| `_acme-challenge.<sub>.clients...` | TXT, efímero | API, durante ACME [F1] |

Todos sin proxy de Cloudflare (nube gris). Los dominios son configurables
(`WGRELAY_BASE_DOMAIN`, `WGRELAY_EDGE_HOST`, `WGRELAY_API_DOMAIN`); no hay
ninguno fijo en el código.

**Los CNAME apuntan al edge, no a IPs.** Sumar o quitar un nodo es editar un
solo registro A; los registros de clientes no cambian nunca. Los dominios
propios de clientes [F3] apuntan al mismo edge.

> ⚠️ **No usar un comodín global `*.clients...`.** Por RFC 4592, al crear el
> TXT de ACME bajo `<sub>`, el nombre `<sub>` pasa a existir como *empty
> non-terminal* y eso **desactiva la síntesis del comodín para él y todo su
> subárbol**: el cliente sacaría su certificado y en ese instante su dominio
> dejaría de resolver. Por eso los registros por tunnel son explícitos.

### 4.3 Alcance del token de Cloudflare

La idea original era delegar `clients.wg-relay.andy.net.ar` como zona propia
con un token limitado a ella. **No es posible en el plan gratuito:** Cloudflare
solo admite zonas de subdominio en el plan Enterprise, y sus tokens no se
pueden limitar a una parte de una zona. Hoy el token puede editar toda
`andy.net.ar`; el código solo escribe bajo el dominio base.

Antes de abrir el servicio a terceros: **mover los clientes a un dominio
dedicado** (una zona propia, con su propio token). Es un cambio de variables
de entorno y un `dns sync`. Coincide con el paso previo a la Public Suffix
List (§6.4).

---

## 5. Ruteo de tráfico

### 5.1 Puerto 443 del nodo

El nodo acepta la conexión, lee el ClientHello (con el propio `crypto/tls`
sobre una conexión de solo lectura, que soporta hellos fragmentados), extrae el
SNI **sin descifrar nada** y resuelve:

1. **Rutas locales** (`WGRELAY_LOCAL_ROUTES`): destinos fijos fuera del túnel.
   La VM de la API la usa para `api.<dominio>` → contenedor de la API.
2. **Tunnels en línea**: match exacto, después comodín subiendo de a una
   etiqueta (`a.b.sub.base` prueba `*.b.sub.base`, `*.sub.base`, ...).
3. Sin match → **se cierra el TCP sin responder**. Un escáner por IP no obtiene
   ni una alerta TLS.

Al encontrar el tunnel, abre una conexión al agente (`<ip-vpn>:443` dentro del
túnel), escribe un header **PROXY v2** con la IP real del visitante, reenvía el
ClientHello y copia bytes en ambos sentidos.

### 5.2 Despacho en el agente

El agente acepta solo conexiones cuyo origen es un gateway de nodo conocido,
lee el header PROXY, lee el SNI y busca el hostname en `wgrelay.yml`:

| Modo | Qué hace el agente | Estado |
|---|---|---|
| `passthrough` | Conecta al `to:` y reenvía el TLS intacto. El servicio termina TLS | ✅ F0 |
| `terminate` (default) | Termina TLS con su certificado y reenvía HTTP al `to:` | F1 |
| `tcp` | Puerto dedicado en el nodo, sin TLS | Futuro |

Hostname sin ruta → se cierra.

### 5.3 IP de origen: PROXY protocol

El header PROXY v2 viaja siempre del nodo al agente. El agente lo reenvía al
servicio **solo si la ruta tiene `proxy_protocol: true`**: un servicio que no
lo espera (un broker MQTT sin configurar) interpretaría el header como basura
y cortaría. En modo `terminate` [F1] el agente lo usa para `X-Forwarded-For`.

### 5.4 Puerto 80

El nodo responde `308` hacia `https://` para hostnames conocidos y corta el
resto sin responder. Es HTTP plano: no compromete el modelo de confianza.

---

## 6. Certificados TLS

Principio: **la clave privada del cliente nunca es legible en nuestros
servidores.**

### 6.1 Certificado de la API

La API saca su propio certificado con TLS-ALPN-01 (`autocert`). El challenge
llega por el `:443` público y el nodo lo reenvía a la API por ruta local sin
tocar el TLS. No depende de Cloudflare.

### 6.2 Dominio asignado, modo terminate [F1]

El agente pide el certificado a Let's Encrypt y resuelve DNS-01 llamando a la
API, que escribe el TXT en Cloudflare (solo dentro de la zona del tunnel
autenticado) y lo borra al terminar. Soporta **wildcard** y es independiente
del data plane.

### 6.3 Almacenamiento de certificados sin volumen [F1]

El agente no tiene disco, pero **no puede sacar un certificado nuevo en cada
arranque**: Let's Encrypt permite solo 5 certificados idénticos por semana.
Un contenedor que se reinicia seis veces en una semana quedaría sin TLS hasta
la semana siguiente.

Solución: `certmagic` guarda certificados, claves y la cuenta ACME a través de
una interfaz `Storage`, que implementamos contra la API **cifrando del lado del
agente**:

```
clave = HKDF(token, "cert storage")        ← solo el agente puede calcularla
blob  = AES-256-GCM(clave, certificado + clave privada)
PUT /v1/agent/storage/{nombre}  blob       ← la API guarda bytes opacos
```

La API solo tiene el hash SHA-256 del token, así que no puede derivar la
clave ni leer lo que guarda. Al reiniciar, el agente descarga y descifra.
Rotar el token vuelve ilegible lo guardado: el agente simplemente emite
certificados nuevos (evento raro, dentro de la cuota).

### 6.4 Límite de Let's Encrypt

LE limita ~50 certificados por semana y por **dominio registrado**, calculado
con la Public Suffix List. Como todos los clientes cuelgan de un mismo dominio:

> **Techo del servicio: ~50 clientes con certificado nuevo por semana.** Sin
> problema para desarrollo y colegas. Bloqueante para abrirlo al público.

Solución: dominio dedicado (§4.3) registrado en la **Public Suffix List**.
Además de levantar el límite, aísla cookies entre clientes y evita que un
cliente abusivo arrastre al dominio padre a las blocklists de Safe Browsing.
Diferido por decisión explícita; el trámite tarda semanas.

### 6.5 Dominio propio del cliente [F3]

TLS-ALPN-01 a través del túnel, en modo terminate. Para wildcard o passthrough
sobre dominio propio, el cliente aporta credenciales de su proveedor DNS.

---

## 7. Autenticación e identidad

### 7.1 Tokens

Formato `wgr_<id>_<secreto>` (agentes) y `wgn_<id>_<secreto>` (nodos). El `id`
permite el lookup; del secreto se guarda **SHA-256**.

No se usa Argon2 (lo proponía la versión anterior de este documento): los
hashes lentos protegen contraseñas de baja entropía elegidas por humanos. Un
secreto de 256 bits aleatorios no es atacable por fuerza bruta, y un hash
lento costaría CPU en cada heartbeat sin ganar nada. Es lo mismo que hace
GitHub con sus tokens.

### 7.2 Por qué NO se usa la clave privada de WireGuard como token

1. Las claves de WireGuard son **Curve25519 para ECDH, no Ed25519 para
   firma**: no se puede firmar con ellas, así que usarlas como credencial
   implica **transmitir la clave privada**. Un volcado de la base filtraría los
   túneles de todos.
2. Acoplaría dos ciclos de vida independientes (rotar la clave WG vs. revocar
   acceso a la API).

### 7.3 Agente sin estado: clave efímera + lease

En cada arranque el agente:

1. Genera un par de claves WireGuard **nuevo, solo en memoria**.
2. Llama a `register` con el token, la clave pública y un `instance_id` aleatorio.
3. La API le otorga el **lease** del tunnel (vence a los 45 s) y le devuelve
   IP, dominio y nodos.
4. Renueva el lease con un heartbeat cada 15 s. Al apagarse, lo libera.

Los nodos solo aceptan como peers a los agentes con lease vigente.

Esto define qué pasa en cada caso, sin estado local:

| Situación | Resultado |
|---|---|
| Reinicio normal (`docker compose restart`) | Libera el lease al apagarse; la nueva instancia lo toma al instante |
| El contenedor muere sin avisar | La nueva instancia espera ≤45 s a que venza el lease |
| Dos servidores con el mismo token | El segundo **queda en espera** y avisa en el log; si el primero cae, toma el túnel solo (failover) |
| Token rotado o revocado | El agente pierde el lease y termina con error claro |

La alternativa (derivar la clave WG del token, determinística) se descartó:
dos instancias con la misma clave harían que WireGuard alterne entre ellas en
silencio. Con el lease, la segunda instancia no tiene sus paquetes aceptados.

### 7.4 Nodos sin estado

La clave WireGuard del nodo se deriva de su token con HKDF. Es estable entre
reinicios sin guardar nada en disco, y el control plane no puede calcularla.

### 7.5 Superficie expuesta

| Componente | Puertos públicos | Secretos que tiene |
|---|---|---|
| Nodo | 443/tcp, 80/tcp, 51820/udp | Su propio token |
| API | Ninguno (entra por el nodo) | Token de Cloudflare, hashes de tokens |
| Postgres | Ninguno | — |
| Agente | Ninguno | Su token |

---

## 8. Modelo de datos

```sql
account      (id, email UNIQUE, plan, created_at)
tunnel       (id, account_id, subdomain UNIQUE, vpn_ip UNIQUE, created_at)
token        (id PK, tunnel_id UNIQUE, secret_hash, created_at, last_used_at)
agent_lease  (tunnel_id PK, instance_id, wg_pubkey UNIQUE, agent_version,
              acquired_at, expires_at)
node         (id, name UNIQUE, endpoint, gateway_ip UNIQUE, token_id UNIQUE,
              token_hash, wg_pubkey, node_version, last_seen_at, created_at)
```

`token.tunnel_id UNIQUE` implementa *un token vigente por tunnel*; rotar es
reemplazar la fila. `agent_lease.wg_pubkey UNIQUE` impide que un agente
registre la clave de otro. Migraciones embebidas en el binario, aplicadas al
arrancar con un advisory lock (seguro con varias réplicas).

[F1] `agent_storage (tunnel_id, name, blob, updated_at)` para §6.3.

---

## 9. API

### Agente (`Authorization: Bearer wgr_...`)

```
POST /v1/agent/register   {instance_id, wg_public_key, version}
                       →  {tunnel_id, vpn_ip, domain, nodes[], lease_ttl_seconds,
                           heartbeat_seconds}             409 lease_held
POST /v1/agent/heartbeat  {instance_id} → {nodes[]}       409 superseded
POST /v1/agent/release    {instance_id} → 204
```

### Nodo (`Authorization: Bearer wgn_...`)

```
POST /v1/node/hello              {wg_public_key, version} → {name, gateway_ip}
GET  /v1/node/config?since=<v>   → {version, peers[], routes[]}   | 304
```

`config` es un long-poll de hasta 25 s. `version` es un hash del contenido:
cambia si y solo si cambia la configuración. La API consulta la base cada
segundo durante la espera (en vez de notificar en memoria): así funciona con
varias réplicas de la API y detecta los leases que vencen sin que nadie
escriba.

Si la API se cae, nodos y agentes conservan su última configuración y el
tráfico sigue fluyendo.

---

## 10. Configuración del cliente

```yaml
# wgrelay.yml — versionable, sin secretos
relay: https://api.wg-relay.andy.net.ar
routes:
  - host: mqtt              # relativo → mqtt.<dominio asignado>; "@" = el dominio
    mode: passthrough
    to: emqx:8883           # admite ${VARIABLES} de entorno
  - host: web
    mode: passthrough
    to: nginx:443
    proxy_protocol: true
```

```sh
# .env — NO versionar
WGRELAY_TOKEN=wgr_...
```

Si el archivo contiene una clave `token:`, el agente se niega a arrancar y
explica por qué: el archivo está pensado para commitearse.

Al arrancar, el agente imprime su dominio, sus nodos y cada ruta con su
destino, y avisa en el log cuando el túnel con cada nodo queda establecido.

El agente se conecta a los `to:` como cualquier proceso: por nombre de
servicio si comparte red Docker con ellos, o a `host.docker.internal` /
IPs de la LAN.

---

## 11. Planes

| | Free | Persistente | Pago (futuro) |
|---|---|---|---|
| Subdominio asignado, estable | ✅ | ✅ | ✅ |
| Rutas por hostname (`passthrough` / `terminate`) | ✅ | ✅ | ✅ |
| Dominio propio | ❌ | ✅ | ✅ |
| Puertos TCP dedicados | ❌ | ❌ | ✅ |
| Vanity subdomain | ❌ | ❌ | ✅ |

### 11.1 "Efímero" es la sesión, no el nombre

Todo tunnel desaparece de los nodos cuando su agente se desconecta (§7.3):
eso ya es el comportamiento efímero, para todos los planes. El **nombre** en
cambio es estable por tunnel. Un subdominio aleatorio por sesión exigiría un
certificado nuevo por sesión y agotaría la cuota de Let's Encrypt de todo el
servicio en horas; además, reciclar nombres dejaría al dueño anterior con un
certificado válido para un nombre ajeno. **Nunca se recicla un subdominio.**

### 11.2 Hostnames escalan, puertos no

Los hostnames comparten el `:443` vía SNI: son pseudoinfinitos, disponibles en
todos los planes. Los puertos dedicados (~65k por nodo, y un cliente necesita
el mismo en todos los nodos: reserva global) son el recurso escaso, reservado
al plan pago.

---

## 12. Escalado horizontal

- Nodos sin estado: convergen a lo que dicta la API. Sumar uno = una VM, dos
  registros DNS y un `node create` ([DEPLOY.md](DEPLOY.md), parte 2).
- Los agentes mantienen túnel con **todos** los nodos a la vez; el registro A
  múltiple del edge reparte el tráfico entrante.
- La API no guarda estado local: se puede replicar detrás de un balanceador
  cuando haga falta. Hoy corre en la VM 1 junto a Postgres.

---

## 13. Fases

| Fase | Entrega | Estado |
|---|---|---|
| **F0** | DB, tokens, leases, túnel WG userspace, router SNI, passthrough, redirect :80, CLI de admin, DNS automático, e2e | ✅ |
| **F1** | Modo `terminate`: ACME DNS-01 delegado, almacén cifrado de certs, `export_cert` | |
| **F2** | Migrar SensorHub | |
| **F3** | Dominios propios con verificación DNS | |
| **F4** | Métricas y cuotas de tráfico; rate limiting de la API | |
| — | Puertos TCP dedicados, UDP, panel web, PSL | Futuro |

---

## 14. Decisiones

| # | Decisión | Alternativa descartada | Razón |
|---|---|---|---|
| 1 | TLS termina en el cliente | Terminar en el relay | Un cliente no debería confiarnos sus claves |
| 2 | Go en los tres componentes | Python/Node, bash | Binario estático, multi-arch, tipos compartidos |
| 3 | Router L4 propio | nginx + config generada | Reconfiguración en caliente, sin reloads |
| 4 | WireGuard userspace (netstack) | WireGuard de kernel | Agente sin privilegios; sin forwarding |
| 5 | Nodo rutea a tunnels, agente a servicios | Rutas por servicio en el nodo | Rutas locales al cliente; tabla del nodo mínima |
| 6 | Agente sin estado: clave efímera + lease | Identidad en volumen; clave derivada del token | Un archivo y un token; failover gratis |
| 7 | Certs cifrados con clave derivada del token, guardados en la API | Volumen en el agente; certs legibles en el servidor | Sin estado local y sin leer claves ajenas |
| 8 | SHA-256 para tokens | Argon2id | Secretos de 256 bits; hash lento no aporta |
| 9 | CNAME por tunnel al edge | Comodín global; A por tunnel | RFC 4592; sumar nodos sin tocar clientes |
| 10 | v1 solo SNI en :443 | TCP/UDP desde el inicio | 90% de los casos con una fracción del trabajo |
| 11 | "Efímero" = sesión, no nombre | Subdominio aleatorio por sesión | Cuota de Let's Encrypt |
| 12 | Un token = un servidor | Varios agentes activos por token | Unidad de aislamiento y cuota |

---

## 15. Pendientes y riesgos

- **Public Suffix List y dominio dedicado** — diferido (§4.3, §6.4).
- **Abuso** — sin política. Antes de abrir a terceros: rate limits, detección
  de phishing, canal de denuncias.
- **Rate limiting de la API** — no implementado. Hoy el acceso requiere un
  token emitido a mano.
- **Punto único de la API** — la API y Postgres viven en una VM. Los túneles
  existentes sobreviven a su caída, pero hace falta backup de Postgres.
- **Límite de registros de Cloudflare** — dos por tunnel; verificar el tope
  del plan antes de crecer.
