# Casos de uso

Qué puede hacer hoy un usuario de wg-relay, qué va a poder hacer, y qué modo
le conviene en cada situación.

Todos los ejemplos exponen el mismo stack:

| Servicio | Protocolo | Puerto típico |
|---|---|---|
| **frontend** | HTTP | 80 |
| **api** | HTTP | 3000 |
| **mqtt** | MQTT (no HTTP) | 1883 sin TLS, 8883 con TLS |

Y se repiten en dos entornos:

- **Entorno D (Docker):** los servicios están en una red de Docker y se
  alcanzan por nombre (`frontend:80`).
- **Entorno L (local):** los servicios escuchan en puertos del host
  (`127.0.0.1:3000`).

---

## 1. Los cuatro modos de exposición

Lo que cambia entre modos es **quién termina el TLS** y **qué protocolo habla
el agente con tu servicio**.

| Modo | `to:` | Termina TLS | Habla con el servicio | Estado |
|---|---|---|---|---|
| `terminate` HTTP | `http://api:3000` | el agente | HTTP plano | ✅ |
| `terminate` TCP | `tcp://emqx:1883` | el agente | TCP plano | ✅ |
| `passthrough` | `emqx:8883` | tu servicio | TLS | ✅ |
| `tcp` (puerto dedicado) | `postgres:5432` | nadie | TCP plano | ❌ futuro (§6.2) |

```
terminate   internet ──TLS──► nodo ──► agente ──[descifra]──► servicio (plano)
passthrough internet ──TLS──► nodo ──► agente ──────────────► servicio (TLS)
```

En **`terminate`** tu servicio no sabe que existe TLS: no maneja certificados,
no los renueva, no se entera. En **`passthrough`** el agente no descifra nada y
tu servicio necesita sus propios certificados.

> El relay **nunca** descifra, en ningún modo. Lo que se decide acá es si el
> TLS termina en el agente (en tu servidor) o más adentro todavía.

---

## 2. Escenario A — dominio asignado por wg-relay

El caso de entrada: el usuario tiene un token y quiere exponer sus servicios
sin tener dominio propio. 

### Cómo obtiene su URI

1. El operador del servicio crea el tunnel y le entrega un token:

   ```sh
   wgrelay-api tunnel create --email usuario@ejemplo.com
   ```
  
  > NOTA: En implementaciones futuras esto deberia poder autogestionarlo el mismo cliente via API


2. El usuario pone el token en `WGRELAY_TOKEN` y levanta el agente.
3. El agente imprime el dominio asignado, por ejemplo
   `dsk7yrh.clients.wg-relay.andy.net.ar`, y una línea por cada ruta.

El dominio es **estable**: le pertenece a ese tunnel para siempre. Los
registros DNS (`<sub>` y `*.<sub>`) los crea el control plane.

Como el comodín ya apunta al agente, el usuario puede inventar subdominios sin
avisarle a nadie: `app.`, `api.`, `mqtt.`, `lo-que-sea.`

### A.D — Docker (el caso más común)

```yaml
# wgrelay.yml
relay: https://api.wg-relay.andy.net.ar
acme:
  email: usuario@ejemplo.com

routes:
  - host: app                    # → https://app.dsk7yrh.clients...
    to: http://frontend:80
  - host: api                    # → https://api.dsk7yrh.clients...
    to: http://api:3000
  - host: mqtt                   # → mqtts://mqtt.dsk7yrh.clients...:443
    to: tcp://emqx:1883
```

```yaml
# docker-compose.yml
services:
  wgrelay:
    image: ghcr.io/andypecotche/wg-relay-agent:latest
    environment: {WGRELAY_TOKEN: "${WGRELAY_TOKEN}"}
    volumes: ["./wgrelay.yml:/etc/wgrelay/wgrelay.yml:ro"]
    networks: [app]              # la misma red que frontend, api y emqx
networks:
  app: {name: mi_proyecto_network, external: true}
```

| | Cubierto hoy |
|---|---|
| frontend y api | ✅ certificados automáticos, cero configuración |
| mqtt | ✅ `terminate` con `to: tcp://`: EMQX no necesita certificados propios |

### A.L — servicios en localhost

Idéntico, cambiando los destinos y cómo se conecta el contenedor:

```yaml
routes:
  - host: app
    to: http://127.0.0.1:8080
  - host: api
    to: http://127.0.0.1:3000
  - host: mqtt
    to: tcp://127.0.0.1:1883
```

```yaml
services:
  wgrelay:
    image: ghcr.io/andypecotche/wg-relay-agent:latest
    network_mode: host           # comparte la red del host
    environment: {WGRELAY_TOKEN: "${WGRELAY_TOKEN}"}
    volumes: ["./wgrelay.yml:/etc/wgrelay/wgrelay.yml:ro"]
```

Alternativas a `network_mode: host`:

- Dejar el contenedor en su red y usar `host.docker.internal`, agregando
  `extra_hosts: ["host.docker.internal:host-gateway"]` (hace falta en Linux).
- Correr el binario suelto, sin Docker: el agente no necesita privilegios ni
  módulos del kernel.

---

## 3. Cómo se obtienen los certificados

Tres caminos, según el modo:

### 3.1 Los saca el agente (modo `terminate`) ✅

El agente pide el certificado a Let's Encrypt con **TLS-ALPN-01**, que viaja
intacto por el túnel: el challenge entra por el `:443` del nodo con el SNI del
cliente y se rutea hasta el agente como cualquier otra conexión. El control
plane no participa.

El usuario no hace nada: ni certbot, ni renovaciones, ni recargas. Solo pone
`acme.email` en su `wgrelay.yml`.

Los certificados se guardan **cifrados en el control plane**, con una clave
derivada del token que el servicio no puede calcular. Por eso el agente no
necesita volúmenes y sobrevive a reinicios sin reemitir.

**Limitación:** TLS-ALPN-01 no emite comodines. Hoy hay que declarar cada
subdominio. Con DNS-01 (§6.1) eso desaparece.

Funciona igual para HTTP y para TCP: `to: http://api:3000` proxea HTTP,
`to: tcp://emqx:1883` termina el TLS y entrega bytes crudos. El certificado
se obtiene de la misma forma en los dos casos; lo único que cambia es qué
hace el agente con la conexión ya descifrada.

### 3.2 Se los damos exportados (`export_cert`) ❌ falta

Planeado para F1b: el agente obtiene un certificado **comodín** vía DNS-01
delegado y deja `fullchain.pem` y `privkey.pem` en una carpeta.

```yaml
routes:
  - host: mqtt
    mode: passthrough
    to: emqx:8883
    export_cert: /certs/mqtt     # el usuario monta esto en EMQX
```

Es la respuesta para quien quiere que su servicio termine el TLS él mismo pero
sin pelearse con certbot.

### 3.3 Se los consigue el usuario (modo `passthrough`) ✅

El usuario pone su propio nginx, Caddy o certbot. Funciona hoy: como el relay
rutea por SNI sin descifrar, el challenge TLS-ALPN-01 de su proxy llega igual.
Ejemplo completo en [`deploy/client/examples/caddy/`](../deploy/client/examples/caddy/).

Es el camino de quien quiere control total, o quien necesita algo que el
agente no hace (compresión, WAF, reglas raras, terminar TLS para un protocolo
que todavía no soportamos).

---

## 4. Escenario B — el usuario trae su propio dominio ❌ futuro (F2)

Alguien con `example.com` pero sin IP pública. Lo que va a tener que hacer:

1. Apuntar sus nombres al edge del servicio, por CNAME:
   ```
   app.example.com    CNAME  wg-relay.andy.net.ar
   api.example.com    CNAME  wg-relay.andy.net.ar
   mqtt.example.com   CNAME  wg-relay.andy.net.ar
   ```
   En el ápex (`example.com` pelado) `CNAME` no es válido: ahí hacen falta
   registros `A` a las IPs de los nodos, o el *CNAME flattening* de Cloudflare.

2. Declararlos en su `wgrelay.yml` como FQDN:
   ```yaml
   routes:
     - host: app.example.com
       to: http://frontend:80
     - host: api.example.com
       to: http://api:3000
   ```

3. El control plane verifica por DNS que esos nombres apuntan al servicio,
   antes de rutearlos. Sin esa verificación, cualquiera podría registrar el
   dominio de un tercero y quedar en posición de interceptarlo el día que ese
   tercero migre hacia nosotros.

**Certificados:** para un hostname puntual en `terminate`, TLS-ALPN-01
funciona igual, sin que el usuario configure nada. Para wildcard, el DNS-01 lo
resuelve **el agente**, no nuestro servidor: la zona es del usuario y no
queremos (ni podemos) tener acceso a ella. Tres formas, de más a menos
automatizada (detalle técnico en [DESIGN.md §6.5](DESIGN.md)):

1. **Token de su proveedor DNS, configurado localmente en el agente.** Nunca
   llega a nuestro servidor. Vía `lego`, sirve para ~150 proveedores.
2. **CNAME delegado** (recomendada): el usuario crea una vez
   `_acme-challenge.example.com CNAME algo.acme.wg-relay.andy.net.ar`, y a
   partir de ahí las renovaciones las resolvemos nosotros en nuestra propia
   zona, sin que el usuario nos dé ninguna credencial ni tenga que volver a
   intervenir.
3. **Manual**: el usuario pega el TXT a mano en su proveedor, cada ~90 días.

| | Estado |
|---|---|
| Rutear un dominio propio | ❌ el agente rechaza FQDN fuera del dominio asignado |
| Verificación de propiedad | ❌ sin implementar |
| Certificados para dominio propio (hostname puntual) | ✅ el mecanismo ya sirve, falta habilitar las rutas |
| Certificados para dominio propio (wildcard) | ❌ requiere DNS-01 del lado del agente |

---

## 5. Escenario C — "dame el comodín y me arreglo solo"

El usuario que no quiere que el agente decida nada: le mandamos todo y él
resuelve con sus herramientas.

```yaml
routes:
  - host: "*"                    # todos los subdominios
    mode: passthrough
    to: caddy:443
    proxy_protocol: true
  - host: "@"                    # el dominio raíz también
    mode: passthrough
    to: caddy:443
    proxy_protocol: true
```

Con esto el agente es un túnel y nada más. Está cubierto hoy y es lo que usa
el ejemplo de Caddy. `proxy_protocol: true` le transmite la IP real del
visitante al proxy del usuario; sin eso, vería siempre la IP del túnel.

---

## 6. Lo que falta

### 6.1 Certificados comodín y `export_cert` ❌ (F1b)

Necesitan DNS-01 delegado: un endpoint en el control plane que escriba el TXT
del challenge en Cloudflare, validando que el nombre cae dentro de la zona del
tunnel autenticado. Habilita:

- Un solo certificado `*.<sub>` para todos los subdominios del usuario.
- `export_cert`, para quien quiera terminar TLS por su cuenta (§3.2).
- Certificados para hostnames en `passthrough`, donde el challenge nunca
  llegaría al agente.

### 6.2 Puertos dedicados (TCP y UDP crudos) ❌ futuro

Todo lo anterior vive sobre el `:443` y se multiplexa por SNI, así que
**requiere que el cliente hable TLS y envíe SNI**. Queda afuera:

- Clientes que no envían SNI (stacks TLS viejos, herramientas incompletas).
- Protocolos sin TLS: MQTT en 1883 puro, Postgres sin TLS, SSH.
- UDP, que no tiene SNI ni conexiones.

La única solución es asignar un puerto dedicado por servicio en el nodo
(`<sub>.clients...:34812` → `postgres:5432`). Es el recurso escaso del sistema
—unos 65k por nodo, y hay que reservar el mismo en todos— así que queda
reservado para un plan pago.

---

## 7. Resumen

| Caso de uso | Estado |
|---|---|
| Frontend y API HTTP con certificados automáticos (Docker o localhost) | ✅ |
| **Exponer MQTT/Postgres con TLS sin configurar nada** (`terminate` + `tcp://`) | ✅ |
| Exponer un servicio TLS propio sin tocarlo (`passthrough`) | ✅ |
| Comodín para que el usuario haga lo que quiera con su propio proxy | ✅ |
| IP real del visitante hacia el backend (PROXY protocol) | ✅ |
| Failover: dos servidores con el mismo token, el segundo espera | ✅ |
| Certificado comodín y `export_cert` | ❌ §6.1 |
| Dominio propio del usuario | ❌ §4 |
| Clientes sin SNI, protocolos sin TLS, UDP | ❌ §6.2 |

### Orden sugerido

1. **§6.1 DNS-01 delegado.** Comodines y `export_cert`.
2. **§4 dominios propios.** Es lo que convierte el servicio en algo usable por
   alguien con una marca.
3. **§6.2 puertos dedicados.** Recién cuando aparezca un caso real que lo pida.
