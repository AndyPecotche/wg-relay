# Despliegue

Guía paso a paso para levantar wg-relay con **un nodo**, y después sumar
**más nodos**. Cada VM solo necesita Docker y una IPv4 pública.

Los ejemplos usan `wgr.com.ar`. Cualquier otro dominio funciona igual: todos
los nombres se configuran en `.env`.

| Nombre | Para qué |
|---|---|
| `api.wg-relay.wgr.com.ar` | API del control plane (agentes y nodos hablan con ella) |
| `wg-relay.wgr.com.ar` | *Edge*: un registro A por cada nodo. Los clientes apuntan acá |
| `node1.wg-relay.wgr.com.ar` | Endpoint WireGuard de cada nodo (uno por nodo) |
| `*.clients.wg-relay.wgr.com.ar` | Subdominios de clientes (los crea la API) |

---

## Parte 1 — Primer servidor (API + nodo 1)

### DNS

En Cloudflare, zona `wgr.com.ar`. **Todos en "DNS only" (nube gris)**: con el
proxy de Cloudflare activado, Cloudflare terminaría el TLS y nada funcionaría.

- [ ] `api.wg-relay` → **A** → IP de la VM 1
- [ ] `wg-relay` → **A** → IP de la VM 1
- [ ] `node1.wg-relay` → **A** → IP de la VM 1

### DNS de `clients.*`: proveedor externo o propio (elegí uno)

Sin ninguno de los dos, cada vez que des de alta un cliente la API te imprime
los registros para crearlos a mano. Dos caminos, ver DESIGN.md §4.3:

#### Opción A — Cloudflare (opcional)

- [ ] Crear token en *My Profile → API Tokens → Create Token → Edit zone DNS*,
      zona `wgr.com.ar`.
- [ ] Copiar el *Zone ID* (panel de la zona, columna derecha, "API").
- [ ] Completar `CLOUDFLARE_API_TOKEN`/`CLOUDFLARE_ZONE_ID` en `.env`.

> Cloudflare no permite limitar un token a un subdominio: este token puede
> editar toda la zona `wgr.com.ar`. El código solo escribe bajo el dominio
> base, pero si en algún momento abrís el servicio a terceros conviene mover
> los clientes a un dominio dedicado (ver DESIGN.md §4.3).

#### Opción B — DNS propio en los nodos (sin token, sin cuota)

Los nodos sirven ellos mismos `clients.wg-relay.wgr.com.ar` como DNS
autoritativo (`internal/dnsserver`, DESIGN.md §4.3.1). El único paso es
delegar ese subdominio por NS — sin glue records, porque los nombres de los
nameservers quedan afuera de lo delegado:

- [ ] En Cloudflare (zona `wgr.com.ar`), crear dos registros A comunes para
      los nodos que van a ser nameservers, ej. `ns1.wg-relay` → IP de node1,
      `ns2.wg-relay` → IP de node2 (con al menos dos, la delegación sigue
      funcionando si uno cae).
- [ ] Crear dos registros **NS** delegando el subdominio:
      `clients.wg-relay` → `ns1.wg-relay.wgr.com.ar.` y
      `ns2.wg-relay.wgr.com.ar.`.
- [ ] En `.env` de la API: `WGRELAY_DNS_NS_NAMES=ns1.wg-relay.wgr.com.ar,ns2.wg-relay.wgr.com.ar`
      y `WGRELAY_DNS_SOA_EMAIL=admin@wgr.com.ar`.
- [ ] En `.env`/compose de **cada** nodo que declaraste como NS (node1, node2):
      descomentar `WGRELAY_DNS_LISTEN` y los puertos `53:5300/udp`+`53:5300/tcp`.
- [ ] En `node create`, pasarle `--public-ip` a **todos** los nodos (no solo a
      los declarados como NS): esa IP es la que se sirve en las respuestas A.
- [ ] Verificar (puede tardar mientras se propaga la delegación en el mundo,
      pero la consulta directa al nameserver no depende de eso):
      ```sh
      dig NS clients.wg-relay.wgr.com.ar @ns1.wg-relay.wgr.com.ar
      dig A cualquiercosa.clients.wg-relay.wgr.com.ar @ns1.wg-relay.wgr.com.ar
      ```

> **`systemd-resolved` suele ocupar el `:53` del host.** Si `docker compose
> up` falla con `failed to bind host port 0.0.0.0:53: address already in
> use`, es casi seguro esto (por eso el contenedor escucha en `:5300` y se
> mapea al `53` público, no al revés). Confirmalo y arreglalo:
> ```sh
> sudo ss -tlnp sport = :53   # debería mostrar "systemd-resolve"
> ```
> Editá `/etc/systemd/resolved.conf`, sección `[Resolve]`, agregá
> `DNSStubListener=no`. Antes de reiniciar, fijate que `/etc/resolv.conf` no
> quede apuntando al stub que estás por apagar (suele ser un symlink a
> `stub-resolv.conf`):
> ```sh
> sudo ln -sf /run/systemd/resolve/resolv.conf /etc/resolv.conf
> sudo systemctl restart systemd-resolved
> resolvectl status && ping -c1 google.com   # confirmá que el host sigue resolviendo
> docker compose up -d --build
> ```
> Si después de esto seguís viendo el mismo error, revisá también que no
> haya otro contenedor viejo (de otro stack en esta misma VPS) publicando el
> `53`: `docker ps -a --format 'table {{.Names}}\t{{.Status}}\t{{.Ports}}'`.

Los dos caminos son independientes: podés tener ambos configurados a la vez
(por ejemplo, mientras migrás de uno a otro), o ninguno.

### Firewall de la VM

- [ ] Abrir **443/tcp**, **80/tcp** y **51820/udp**. Nada más: Postgres y la
      API no se publican.

### Levantar

```sh
git clone https://github.com/AndyPecotche/wg-relay.git && cd wg-relay/deploy/server
cp .env.example .env
```

- [ ] Completar `.env`: dominios, `WGRELAY_ACME_EMAIL`, `POSTGRES_PASSWORD`
      (`openssl rand -hex 24`) y, si lo tenés, lo de Cloudflare.
- [ ] Levantar la API sola:
  ```sh
  docker compose up -d --build postgres api
  ```
- [ ] Crear el nodo y copiar el token que imprime a `WGRELAY_NODE_TOKEN` en `.env`
      (`--public-ip` es opcional, hace falta solo para DNS propio, opción B de
      arriba):
  ```sh
  docker compose exec api wgrelay-api node create --name node1 --endpoint node1.wg-relay.wgr.com.ar:51820 --public-ip 203.0.113.10
  ```
- [ ] Levantar todo:
  ```sh
  docker compose up -d --build
  ```
- [ ] Verificar (la primera vez tarda unos segundos mientras saca el certificado):
  ```sh
  curl https://api.wg-relay.wgr.com.ar/healthz        # → ok
  docker compose exec api wgrelay-api node list        # node1, "0s atrás"
  ```

> Mientras probás, poné en `.env`
> `WGRELAY_ACME_CA=https://acme-staging-v02.api.letsencrypt.org/directory`
> para no gastar cuota de Let's Encrypt (el certificado no será confiable:
> usá `curl -k`). Borrá esa línea y el volumen `apidata` al pasar a producción.

### Dar de alta un cliente

```sh
docker compose exec api wgrelay-api tunnel create --email colega@ejemplo.com
```

Imprime el dominio asignado y el token (**una sola vez**). Si hay un proveedor
DNS externo configurado (Cloudflare/webhook), los registros se crean solos;
con DNS propio no hace falta crear nada (ya es resoluble); sin ninguno de los
dos, los imprime para crearlos a mano.

Otros comandos:

```sh
docker compose exec api wgrelay-api tunnel list
docker compose exec api wgrelay-api tunnel rotate-token --id 3   # si el token se filtró
docker compose exec api wgrelay-api dns sync                     # recrear DNS de todos
```

---

## Parte 2 — Nodos adicionales (node2, node3, ...)

Por cada VM nueva:

### DNS

- [ ] `node2.wg-relay` → **A** → IP de la VM 2
- [ ] `wg-relay` → **agregar otro A** → IP de la VM 2 (quedan dos registros A
      con el mismo nombre: así se reparte el tráfico entre nodos)

### En la VM de la API

- [ ] Crear el nodo y guardar el token (`--public-ip` si usás DNS propio,
      opción B de la Parte 1 — todos los nodos cuentan para el set de IPs que
      contesta, no solo los declarados como nameserver):
  ```sh
  docker compose exec api wgrelay-api node create --name node2 --endpoint node2.wg-relay.wgr.com.ar:51820 --public-ip 203.0.113.11
  ```

### En la VM nueva

- [ ] Abrir **443/tcp**, **80/tcp**, **51820/udp**.
- [ ] Levantar el nodo:
  ```sh
  git clone https://github.com/AndyPecotche/wg-relay.git && cd wg-relay/deploy/node
  cp .env.example .env      # completar WGRELAY_API_DOMAIN y WGRELAY_NODE_TOKEN
  docker compose up -d --build
  ```

Los agentes ya conectados detectan el nodo nuevo en el siguiente heartbeat
(≤15 s) y abren un túnel también hacia él. No hay que tocar nada del lado
cliente.

### Qué pasa si se cae algo

- **Se cae un nodo:** los navegadores y clientes que resolvieron su IP fallan
  hasta que reintentan con otra del registro `wg-relay`. Los otros nodos siguen
  funcionando. Sacá su IP del registro A si la caída va a ser larga.
- **Se cae la API:** los túneles existentes siguen funcionando (nodos y agentes
  conservan la última configuración). No se pueden conectar agentes nuevos.
  Al volver, puede haber un corte de hasta 15 s mientras los agentes renuevan.
- La API y Postgres viven solo en la VM 1: hacé backup de Postgres
  (`docker compose exec postgres pg_dump -U wgrelay wgrelay > backup.sql`).

---

## Parte 3 — Del lado del cliente

Esto es lo que le pasás a quien quiera usar el servicio. Ver
[`deploy/client/`](../deploy/client/):

1. `wgrelay.yml` con sus rutas (se puede commitear al repo del proyecto).
2. `.env` con `WGRELAY_TOKEN=...` (**no** se commitea).
3. `docker compose up -d`.

El agente no guarda nada en disco: no necesita volúmenes, ni privilegios, ni
`NET_ADMIN`, ni módulo de kernel de WireGuard. Funciona igual en Docker
Desktop (Mac/Windows) o como binario suelto.

---

## Imágenes

Hasta que haya imágenes publicadas, todo se construye desde el código con
`--build`. El workflow `.github/workflows/images.yml` publica
`ghcr.io/andypecotche/wg-relay-{api,node,agent}` en cada push a `main` y en
cada tag `vX.Y.Z`.

- [ ] Tras la primera publicación, en GitHub → *Packages* → cada paquete →
      *Package settings* → **Change visibility → Public**. Sin esto, los
      clientes no pueden descargar la imagen del agente.
