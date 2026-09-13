# wg-relay

> **VPS Ingress Relay & Reverse Proxy Multi-Proyecto Modular con WireGuard, Nginx y Certbot (Cloudflare DNS-01).**

`wg-relay` es una solución contenerizada y lista para producción que convierte uno o más VPS públicos en gateways de entrada (*ingress proxies*) con terminación TLS centralizada. Permite exponer hacia Internet servicios web (HTTP/HTTPS) y flujos de telemetría IoT (MQTTS/TCP) alojados en redes privadas locales detrás de **CGNAT** (sin IP pública ni apertura de puertos), garantizando un **aislamiento perimetral estricto (Zero-Trust L3)** entre proyectos.

---

## Índice

1. [Arquitectura del Sistema](#arquitectura-del-sistema)
2. [Estructura del Repositorio](#estructura-del-repositorio)
3. [Requisitos Previos](#requisitos-previos)
4. [Despliegue Inicial en un VPS Limpio](#despliegue-inicial-en-un-vps-limpio)
5. [Flujo de Trabajo: Agregar un Nuevo Proyecto](#flujo-de-trabajo-agregar-un-nuevo-proyecto)
6. [Aislamiento de Red y Seguridad (iptables)](#aislamiento-de-red-y-seguridad-iptables)
7. [Alta Disponibilidad y Redundancia Multi-VPS](#alta-disponibilidad-y-redundancia-multi-vps)
8. [Comandos de Operación y Mantenimiento](#comandos-de-operación-y-mantenimiento)

---

## Arquitectura del Sistema

```mermaid
flowchart TD
    subgraph Internet ["🌐 Internet Público"]
        ClientWeb["Navegador / Cliente API (HTTPS :443)"]
        ClientMQTT["Dispositivo IoT / Sensor (MQTTS :8883)"]
        CF_DNS["Cloudflare DNS (Proxy / Wildcard *.tudominio.com)"]
    end

    subgraph VPS ["🖥️ Relay VPS (wg-relay)"]
        subgraph DockerNet ["Red Docker Bridge Estática (172.28.0.0/16)"]
            Nginx["Nginx Reverse Proxy\nIP: 172.28.0.10\n- L7: conf.d/*.conf\n- L4: stream.d/*.conf\n- Terminación TLS"]
            WG_Server["WireGuard Gateway\nIP: 172.28.0.2\nVPN: 10.10.0.1/16\n- Firewall: wg0 -> wg0 REJECT"]
            Certbot["Certbot Daemon\nIP: 172.28.0.20\n- DNS-01 Cloudflare"]
        end
    end

    subgraph CGNAT_Area ["🔒 Redes Privadas / Detrás de CGNAT (Sin IP Pública)"]
        subgraph Proj1 ["Proyecto: SensorHub (10.10.1.2)"]
            WG_Peer1["WireGuard Client\n(PersistentKeepalive=25)"]
            Web1["Dashboard Web (:80)"]
            Broker1["EMQX MQTT (:1883)"]
        end

        subgraph Proj2 ["Proyecto 2: Telemetría (10.10.2.2)"]
            WG_Peer2["WireGuard Client\n(PersistentKeepalive=25)"]
            API2["Backend API (:3000)"]
        end
    end

    ClientWeb -->|HTTPS :443| Nginx
    ClientMQTT -->|MQTTS :8883| Nginx
    Certbot <-->|DNS-01 API| CF_DNS

    Nginx -->|Ruta L3: 10.10.0.0/16| WG_Server
    WG_Server -->|Túnel UDP 51820| WG_Peer1
    WG_Server -->|Túnel UDP 51820| WG_Peer2

    WG_Peer1 --> Web1
    WG_Peer1 --> Broker1
    WG_Peer2 --> API2

    %% Aislamiento
    WG_Peer1 -.x|BLOQUEADO POR IPTABLES| WG_Peer2
```

---

## Estructura del Repositorio

```
wg-relay/
├── .env.example                        # Plantilla de variables de entorno globales
├── .gitignore                           # Exclusión de secretos, claves y certificados
├── docker-compose.yml                  # Definición de servicios (wireguard, nginx, certbot)
├── README.md                           # Documentación completa y manual de operaciones
├── certbot/
│   ├── cloudflare.ini.example          # Plantilla del Token de API de Cloudflare
│   └── init-cert.sh                    # Script de solicitud inicial de certificado Wildcard
├── nginx/
│   ├── nginx.conf                      # Configuración base con soporte L7 (http) y L4 (stream)
│   ├── conf.d/
│   │   └── sensorhub.conf.example      # Ejemplo L7: VirtualHost HTTPS con WebSockets
│   └── stream.d/
│       └── sensorhub_mqtt.conf.example # Ejemplo L4: Terminación TLS TCP para Broker MQTT
├── wireguard/
│   ├── assemble.sh                     # Compilador idempotente de peers y sync en caliente
│   ├── templates/
│   │   └── interface.conf              # Configuración base wg0 + reglas de firewall iptables
│   └── peers.d/
│       └── sensorhub.conf.example      # Ejemplo de configuración modular de un peer
└── scripts/
    ├── add-peer.sh                     # Asistente interactivo/CLI para registrar un nuevo proyecto
    └── reload.sh                       # Recarga en caliente sin downtime (Nginx + WireGuard)
```

---

## Requisitos Previos

- **Servidor VPS:** 1 vCPU, 1 GB RAM con Linux (Ubuntu 22.04 LTS / 24.04 LTS o Debian 12 recomendados).
- **Dominio en Cloudflare:** Dominio administrado en Cloudflare con nameservers activos.
- **Docker Engine y Docker Compose:** Docker v24+ y Compose v2+.
- **Puertos del VPS abiertos en el Firewall del Proveedor:**
  - `80/TCP` (HTTP Ingress / Redirección HTTPS)
  - `443/TCP` (HTTPS Ingress L7)
  - `8883/TCP` (MQTTS TLS Ingress L4)
  - `51820/UDP` (WireGuard VPN Handshake)

---

## Despliegue Inicial en un VPS Limpio

### Paso 1: Preparación del Servidor

Conéctate al VPS por SSH y asegúrate de tener el kernel y Docker al día:

```bash
# Actualizar el sistema e instalar utilidades básicas
sudo apt update && sudo apt upgrade -y
sudo apt install -y curl git ufw wireguard-tools

# Instalar Docker oficial si aún no está instalado
curl -fsSL https://get.docker.com | sudo sh
sudo usermod -aG docker $USER

# Configurar Firewall UFW
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow 22/tcp comment 'SSH'
sudo ufw allow 80/tcp comment 'HTTP Ingress'
sudo ufw allow 443/tcp comment 'HTTPS Ingress'
sudo ufw allow 8883/tcp comment 'MQTTS Ingress'
sudo ufw allow 51820/udp comment 'WireGuard VPN'
sudo ufw enable
```

> **Nota:** Cierra y vuelve a abrir tu sesión SSH si acabas de agregar tu usuario al grupo `docker`.

### Paso 2: Clonar el Repositorio y Configurar Variables

```bash
git clone https://github.com/tu-usuario/wg-relay.git
cd wg-relay

# Configurar variables de entorno
cp .env.example .env
nano .env
```

Ajusta al menos:
- `BASE_DOMAIN`: Tu dominio raíz (ej. `midominio.com`).
- `CERTBOT_EMAIL`: Tu correo para notificaciones de expiración.

### Paso 3: Configurar Credenciales de Cloudflare

Crea un **API Token** en Cloudflare:
1. Dirígete a [Cloudflare Dashboard -> My Profile -> API Tokens](https://dash.cloudflare.com/profile/api-tokens).
2. Haz clic en **Create Token** -> **Create Custom Token**.
3. Permisos: `Zone` -> `DNS` -> `Edit`.
4. Zone Resources: `Include` -> `Specific zone` -> `tu_dominio.com`.
5. Copia el token generado y configúralo:

```bash
cp certbot/cloudflare.ini.example certbot/cloudflare.ini
nano certbot/cloudflare.ini
```

Pega el token:
```ini
dns_cloudflare_api_token = TU_TOKEN_DE_CLOUDFLARE_AQUI
```

Asegura permisos estrictos:
```bash
chmod 600 certbot/cloudflare.ini
```

### Paso 4: Emitir el Certificado Wildcard Inicial

Ejecuta el script automatizado para obtener los certificados `tudominio.com` y `*.tudominio.com` a través del desafío DNS-01:

```bash
chmod +x certbot/init-cert.sh wireguard/assemble.sh scripts/*.sh
./certbot/init-cert.sh
```

El script verificará el token con Cloudflare, creará el registro TXT temporal `_acme-challenge` y guardará los certificados en `certbot/conf/live/tudominio.com/`.

### Paso 5: Iniciar los Servicios Base

```bash
docker compose up -d
```

Verifica que todos los contenedores estén en estado saludable:
```bash
docker compose ps
```

---

## Flujo de Trabajo: Agregar un Nuevo Proyecto

Para incorporar un proyecto (por ejemplo, el gateway del proyecto **SensorHub**):

### 1. Registrar el Peer en WireGuard

Ejecuta el asistente interactivo:

```bash
./scripts/add-peer.sh sensorhub 10.10.1.2
```

El script:
1. Genera un par de claves WireGuard para el cliente si no especificas una.
2. Crea de forma aislada el archivo `wireguard/peers.d/sensorhub.conf`.
3. Sincroniza la configuración del kernel (`wg syncconf`) en caliente sin desconectar otros peers ni reiniciar el contenedor.
4. Imprime en pantalla el bloque de configuración listo para pegar en el cliente.

### 2. Configurar el Cliente Remoto (Detrás de CGNAT)

En el servidor, Raspberry Pi o Edge Gateway del proyecto SensorHub:

1. Instala WireGuard: `sudo apt install -y wireguard`
2. Crea `/etc/wireguard/wg0.conf` con el bloque que imprimió `add-peer.sh`:

```ini
[Interface]
PrivateKey = <CLAVE_PRIVADA_GENERADA_POR_ADD_PEER>
Address = 10.10.1.2/16

[Peer]
PublicKey = <CLAVE_PUBLICA_DEL_VPS>
Endpoint = vps.tudominio.com:51820
AllowedIPs = 10.10.0.0/16
PersistentKeepalive = 25
```

> [!IMPORTANT]
> **`PersistentKeepalive = 25`** es mandatorio. Dado que el nodo remoto está detrás de CGNAT, no tiene IP pública entrante. Esta directiva envía un paquete UDP cada 25 segundos para mantener abierta la tabla de traducción de estados del NAT del ISP.

3. Inicia y habilita WireGuard en el cliente:
```bash
sudo systemctl enable --now wg-quick@wg0
```

4. Prueba la conectividad hacia el VPS:
```bash
ping 10.10.0.1
```

### 3. Exponer Servicios en Nginx

#### Opción A: Exponer Servicio Web / API / Dashboard (L7 HTTP/HTTPS)

Copia la plantilla a un archivo activo `.conf`:

```bash
cp nginx/conf.d/sensorhub.conf.example nginx/conf.d/sensorhub.conf
nano nginx/conf.d/sensorhub.conf
```

Ajusta el `server_name` (`sensorhub.tudominio.com`) y el `proxy_pass http://10.10.1.2:80;`.

#### Opción B: Exponer Broker MQTT con TLS (L4 Stream TCP)

Copia la plantilla de stream:

```bash
cp nginx/stream.d/sensorhub_mqtt.conf.example nginx/stream.d/sensorhub_mqtt.conf
nano nginx/stream.d/sensorhub_mqtt.conf
```

Ajusta el `proxy_pass 10.10.1.2:1883;`.

### 4. Recargar Nginx y WireGuard en Caliente

Aplica los cambios sin caída de servicio (*Zero Downtime*):

```bash
./scripts/reload.sh
```

El script valida la sintaxis con `nginx -t` antes de aplicar `nginx -s reload`. Si hay algún error tipográfico, no afectará a los servicios que ya están en producción.

---

## Aislamiento de Red y Seguridad (iptables)

Para garantizar que un cliente comprometido en el proyecto A no pueda escanear ni acceder a los recursos del proyecto B, `interface.conf` aplica las siguientes reglas a nivel de kernel:

```ini
# 1. BLOQUEO LATERAL ABSOLUTO
PostUp = iptables -I FORWARD -i wg0 -o wg0 -j REJECT

# 2. ACCESO PERMITIDO SOLO DESDE EL REVERSE PROXY
PostUp = iptables -A FORWARD -i eth0 -o wg0 -j ACCEPT
PostUp = iptables -A FORWARD -i wg0 -o eth0 -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT

# 3. MASQUERADE
PostUp = iptables -t nat -A POSTROUTING -o wg0 -j MASQUERADE
```

### ¿Cómo funciona el flujo de paquetes?

1. **Cliente Externo -> Web:**
   El usuario se conecta a `https://sensorhub.tudominio.com`. Nginx (en `172.28.0.10`) procesa el TLS y envía la petición hacia `10.10.1.2`. El kernel del contenedor Nginx sigue la ruta estática hacia la IP de WireGuard (`172.28.0.2`), ingresando a la interfaz `eth0` de WireGuard y reenviándose por el túnel `wg0` hacia el cliente.
2. **Tráfico entre Peers (10.10.1.2 -> 10.10.2.2):**
   Si `10.10.1.2` intenta hacer ping o conectarse a `10.10.2.2`, el paquete entra por `wg0` y pretende salir por `wg0`. La regla `FORWARD -i wg0 -o wg0 -j REJECT` descarta y rechaza la conexión de forma inmediata.

---

## Alta Disponibilidad y Redundancia Multi-VPS

La arquitectura es completamente sin estado (*stateless*), lo que permite clonar el repositorio en un segundo VPS (`wg-relay-02`) para tolerancia a fallos:

```
                  +--------------------------------+
                  |  Cloudflare DNS / Load Balancer |
                  |    relay.tudominio.com         |
                  +---------------+----------------+
                                  |
               +------------------+------------------+
               | (Round Robin / Failover)            |
               v                                     v
      +-----------------+                   +-----------------+
      |  VPS 1 (Activo) |                   |  VPS 2 (Backup) |
      |  IP: 198.51.100.1 |                 |  IP: 203.0.113.1 |
      |  wg0: 10.10.0.1 |                   |  wg0: 10.20.0.1 |
      +--------+--------+                   +--------+--------+
               |                                     |
               +------------------+------------------+
                                  | (Túnel Dual)
                                  v
                    +---------------------------+
                    | Edge Gateway (SensorHub)  |
                    | wg0 -> VPS 1              |
                    | wg1 -> VPS 2              |
                    +---------------------------+
```

### Pasos para replicar en un segundo VPS:

1. **Clonar en el VPS 2:**
   Clona el repositorio en el nuevo servidor.
2. **Configurar `.env` en VPS 2:**
   Puedes usar una subred VPN separada para evitar conflictos (ej. `VPN_SUBNET=10.20.0.0/16` y `VPN_GATEWAY_IP=10.20.0.1`).
3. **Obtener Certificados en VPS 2:**
   Ejecuta `./certbot/init-cert.sh` con el mismo token de Cloudflare.
4. **Configurar el Cliente Dual en el Edge:**
   En el dispositivo del cliente (ej. SensorHub), configura dos interfaces WireGuard:
   - `wg0.conf`: Conexión hacia VPS 1 (`10.10.1.2`).
   - `wg1.conf`: Conexión hacia VPS 2 (`10.20.1.2`).
5. **Configurar Cloudflare:**
   - **DNS Round Robin:** Agrega dos registros `A` para `*.tudominio.com` apuntando a las IPs públicas de ambos VPS. Cloudflare distribuirá las peticiones entre ambos nodos automáticamente.
   - **Cloudflare Load Balancer:** Configura un monitor de salud (Health Check) HTTP/HTTPS en el endpoint `/healthz` de Nginx para conmutación por error automática (*Automatic Failover*) en menos de 5 segundos ante caídas de un centro de datos.

---

## Comandos de Operación y Mantenimiento

### Ver estado de conexiones y handshakes de WireGuard:
```bash
docker compose exec wireguard wg show
```

### Ver logs de Nginx en tiempo real:
```bash
docker compose logs -f nginx
```

### Ver logs del proxy de streams (MQTTS):
```bash
docker compose exec nginx tail -f /var/log/nginx/stream_access.log
```

### Forzar renovación de certificados Let's Encrypt:
```bash
docker compose run --rm certbot certonly --dns-cloudflare \
  --dns-cloudflare-credentials /etc/letsencrypt/cloudflare.ini \
  --force-renewal -d "tudominio.com" -d "*.tudominio.com"
./scripts/reload.sh --nginx-only
```

### Backup rápido de claves y certificados:
```bash
tar -czvf wg-relay-backup-$(date +%F).tar.gz \
  .env \
  certbot/cloudflare.ini \
  certbot/conf/ \
  wireguard/config/ \
  wireguard/peers.d/ \
  nginx/conf.d/ \
  nginx/stream.d/
```
