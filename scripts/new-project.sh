#!/bin/bash
# ==============================================================================
# new-project.sh: Asistente Integral para Alta Automatizada de Proyectos
# ==============================================================================
# Automatiza:
# 1. Asignación automática de IP en la VPN (10.10.x.y)
# 2. Generación de claves WireGuard y archivo en wireguard/peers.d/<IP>-<proyecto>.conf
# 3. Creación de VirtualHost Nginx L7 en nginx/conf.d/<IP>-<dominio>.conf
# 4. Configuración opcional de TCP Stream en nginx/stream.d/<PUERTO>-<dominio>.conf
#    con asignación automática de puerto de escucha (8083 a 50000)
# 5. Verificación y emisión opcional de certificados TLS con Cloudflare DNS-01
# 6. Recarga en caliente sin downtime de WireGuard y Nginx
# 7. Entrega de configuración lista para el cliente local
#
# Uso: ./new-project.sh [nombre_proyecto] [dominio_base]
# Ejemplo: ./new-project.sh sensorhub sensorhub.andy.net.ar
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

PEERS_DIR="${REPO_ROOT}/wireguard/peers.d"
CONF_DIR="${REPO_ROOT}/nginx/conf.d"
STREAM_DIR="${REPO_ROOT}/nginx/stream.d"
CERT_LIVE_DIR="${REPO_ROOT}/certbot/conf/live"
ENV_FILE="${REPO_ROOT}/.env"

# Colores
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

echo -e "${BLUE}${BOLD}=== [wg-relay] Asistente Integral de Alta de Proyectos ===${NC}\n"

# Cargar variables de entorno
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC2046
    export $(grep -v '^#' "${ENV_FILE}" | xargs)
fi

VPS_ENDPOINT_HOST="${VPS_ENDPOINT_HOST:-TU_IP_O_DOMINIO_VPS}"
VPN_PORT="${VPN_PORT:-51820}"
VPN_GATEWAY_IP="${VPN_GATEWAY_IP:-10.10.0.1}"

# ------------------------------------------------------------------------------
# 1. Obtener Nombre del Proyecto y Dominio Base
# ------------------------------------------------------------------------------
PROJECT_NAME="${1:-}"
if [ -z "${PROJECT_NAME}" ]; then
    read -r -p "Nombre del proyecto (ej. sensorhub, telemetry, iot-node): " PROJECT_NAME
fi

# Sanitizar nombre (minúsculas, alfanumérico, guiones)
PROJECT_NAME=$(echo "${PROJECT_NAME}" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]_-')

if [ -z "${PROJECT_NAME}" ]; then
    echo -e "${RED}[!] ERROR: El nombre del proyecto no puede estar vacío.${NC}" >&2
    exit 1
fi

PROJECT_DOMAIN="${2:-}"
if [ -z "${PROJECT_DOMAIN}" ]; then
    read -r -p "Dominio base para el proyecto (ej. ${PROJECT_NAME}.andy.net.ar): " PROJECT_DOMAIN
fi

# Sanitizar dominio
PROJECT_DOMAIN=$(echo "${PROJECT_DOMAIN}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')
PROJECT_DOMAIN="${PROJECT_DOMAIN#\*.}" # Quitar prefijo *. si fue ingresado

if [ -z "${PROJECT_DOMAIN}" ]; then
    echo -e "${RED}[!] ERROR: El dominio no puede estar vacío.${NC}" >&2
    exit 1
fi

echo -e "\n${CYAN}[i] Proyecto:${NC} ${BOLD}${PROJECT_NAME}${NC}"
echo -e "${CYAN}[i] Dominio:${NC}  ${BOLD}${PROJECT_DOMAIN}${NC} (y comodín *.${PROJECT_DOMAIN})"

# ------------------------------------------------------------------------------
# 2. Algoritmo de Auto-asignación de IP VPN (10.10.x.y)
# ------------------------------------------------------------------------------
find_next_vpn_ip() {
    # Recolectar todas las IPs ya asignadas en peers.d y conf.d
    local assigned_ips
    assigned_ips=$(find "${PEERS_DIR}" "${CONF_DIR}" -type f -name "*.conf" 2>/dev/null | \
                   xargs grep -rhoE '10\.10\.[0-9]+\.[0-9]+' 2>/dev/null || true)
    local file_ips
    file_ips=$(find "${PEERS_DIR}" "${CONF_DIR}" -type f -name "10.10.*" 2>/dev/null | \
               sed -n 's/.*\(10\.10\.[0-9]\+\.[0-9]\+\).*/\1/p' || true)
    
    local all_ips="${assigned_ips} ${file_ips} ${VPN_GATEWAY_IP}"

    # Buscar primer hueco secuencial empezando en 10.10.1.2
    for x in $(seq 1 254); do
        for y in $(seq 2 254); do
            local candidate="10.10.${x}.${y}"
            if ! echo "${all_ips}" | grep -qw "${candidate}"; then
                echo "${candidate}"
                return 0
            fi
        done
    done
    return 1
}

AUTO_IP=$(find_next_vpn_ip)
echo -e "${GREEN}[+] IP disponible detectada:${NC} ${BOLD}${AUTO_IP}${NC}"
read -r -p "Presiona Enter para usar ${AUTO_IP} o ingresa una IP diferente: " CUSTOM_IP
PEER_IP="${CUSTOM_IP:-${AUTO_IP}}"
PEER_IP="${PEER_IP%/*}"

# Validar formato
if [[ ! "${PEER_IP}" =~ ^10\.10\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    echo -e "${YELLOW}[!] AVISO: '${PEER_IP}' no sigue el formato habitual 10.10.x.y.${NC}"
fi

# ------------------------------------------------------------------------------
# 3. Generación de Claves WireGuard y Archivo de Peer
# ------------------------------------------------------------------------------
mkdir -p "${PEERS_DIR}"
PEER_FILE="${PEERS_DIR}/${PEER_IP}-${PROJECT_NAME}.conf"

if [ -f "${PEER_FILE}" ]; then
    echo -e "${RED}[!] ERROR: Ya existe un archivo de peer en ${PEER_FILE}.${NC}" >&2
    exit 1
fi

echo -e "${CYAN}[+] Generando par de claves WireGuard para el cliente...${NC}"
if command -v wg >/dev/null 2>&1; then
    CLIENT_PRIVKEY=$(wg genkey)
    CLIENT_PUBKEY=$(echo "${CLIENT_PRIVKEY}" | wg pubkey)
else
    CLIENT_PRIVKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard wg genkey | tr -d '\r\n')
    CLIENT_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard /bin/sh -c "echo '${CLIENT_PRIVKEY}' | wg pubkey" | tr -d '\r\n')
fi

# Guardar peer modular
cat <<EOF > "${PEER_FILE}"
[Peer]
# Proyecto: ${PROJECT_NAME}
# Dominio: ${PROJECT_DOMAIN}
# Fecha de creación: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
PublicKey = ${CLIENT_PUBKEY}
AllowedIPs = ${PEER_IP}/32
EOF

chmod 600 "${PEER_FILE}"
echo -e "${GREEN}[✓] Peer WireGuard creado:${NC} ${PEER_FILE}"

# ------------------------------------------------------------------------------
# 4. Creación de VirtualHost Nginx Layer 7 (conf.d)
# ------------------------------------------------------------------------------
mkdir -p "${CONF_DIR}"
NGINX_CONF_FILE="${CONF_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf"

cat <<EOF > "${NGINX_CONF_FILE}"
# ==============================================================================
# Ingress L7: ${PROJECT_NAME} (${PROJECT_DOMAIN})
# IP VPN Asignada: ${PEER_IP}
# ==============================================================================

server {
    listen 80;
    server_name ${PROJECT_DOMAIN} *.${PROJECT_DOMAIN};

    location / {
        return 301 https://\$host\$request_uri;
    }
}

server {
    listen 443 ssl;
    http2 on;
    server_name ${PROJECT_DOMAIN} *.${PROJECT_DOMAIN};

    ssl_certificate     /etc/letsencrypt/live/${PROJECT_DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${PROJECT_DOMAIN}/privkey.pem;

    add_header X-Frame-Options "SAMEORIGIN" always;
    add_header X-XSS-Protection "1; mode=block" always;
    add_header X-Content-Type-Options "nosniff" always;
    add_header Referrer-Policy "no-referrer-when-downgrade" always;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;

    client_max_body_size 64M;

    location / {
        proxy_pass http://${PEER_IP}:80;

        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection \$connection_upgrade;
        proxy_set_header Host \$host;
        proxy_set_header X-Real-IP \$remote_addr;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_set_header X-Forwarded-Host \$host;
        proxy_set_header X-Forwarded-Port \$server_port;

        proxy_connect_timeout 60s;
        proxy_send_timeout 60s;
        proxy_read_timeout 60s;
    }
}
EOF

echo -e "${GREEN}[✓] VirtualHost Nginx L7 creado:${NC} ${NGINX_CONF_FILE}"

# ------------------------------------------------------------------------------
# 5. Configuración Opcional de TCP Stream Layer 4 (stream.d)
# ------------------------------------------------------------------------------
find_next_stream_port() {
    local assigned_ports
    assigned_ports=$(find "${STREAM_DIR}" -type f -name "*.conf" 2>/dev/null | \
                     sed -n 's/.*\/[0-9]*\/\?\([0-9]\+\)-.*/\1/p' || true)
    local config_ports
    config_ports=$(find "${STREAM_DIR}" -type f -name "*.conf" 2>/dev/null | \
                   xargs grep -rhoE 'listen[[:space:]]+[0-9]+' 2>/dev/null | awk '{print $2}' || true)
    local all_ports="${assigned_ports} ${config_ports} 1883 8883"

    for port in $(seq 8083 50000); do
        if ! echo "${all_ports}" | grep -qw "${port}"; then
            echo "${port}"
            return 0
        fi
    done
    return 1
}

STREAM_PORT=""
STREAM_INTERNAL_PORT=""
echo -e "\n${BOLD}¿Deseas configurar un proxy TCP (Stream) para este proyecto?${NC} (ej. MQTT, DB, Socket)"
read -r -p "Habilitar proxy TCP? (s/N): " ENABLE_STREAM

if [[ "$ENABLE_STREAM" =~ ^([sS][iI]|[sS])$ ]]; then
    mkdir -p "${STREAM_DIR}"
    AUTO_STREAM_PORT=$(find_next_stream_port)
    echo -e "${GREEN}[+] Siguiente puerto de escucha TCP disponible:${NC} ${BOLD}${AUTO_STREAM_PORT}${NC}"
    read -r -p "Presiona Enter para usar ${AUTO_STREAM_PORT} o indica otro puerto: " USER_STREAM_PORT
    STREAM_PORT="${USER_STREAM_PORT:-${AUTO_STREAM_PORT}}"

    read -r -p "Puerto TCP interno en el cliente remoto (ej. 1883 para MQTT, 5432 para DB) [default: 1883]: " USER_INT_PORT
    STREAM_INTERNAL_PORT="${USER_INT_PORT:-1883}"

    STREAM_FILE="${STREAM_DIR}/${STREAM_PORT}-${PROJECT_DOMAIN}.conf"

    cat <<EOF > "${STREAM_FILE}"
# ==============================================================================
# Stream TCP L4: ${PROJECT_NAME} (${PROJECT_DOMAIN})
# Escucha pública TLS: ${STREAM_PORT} -> Destino remoto: ${PEER_IP}:${STREAM_INTERNAL_PORT}
# ==============================================================================

server {
    listen ${STREAM_PORT} ssl;

    ssl_certificate     /etc/letsencrypt/live/${PROJECT_DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${PROJECT_DOMAIN}/privkey.pem;

    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers HIGH:!aNULL:!MD5;
    ssl_session_cache shared:SSL_TCP_${PROJECT_NAME}_${STREAM_PORT}:10m;
    ssl_session_timeout 4h;

    proxy_timeout 1h;
    proxy_connect_timeout 10s;

    proxy_pass ${PEER_IP}:${STREAM_INTERNAL_PORT};
}
EOF
    echo -e "${GREEN}[✓] Proxy TCP Stream L4 creado:${NC} ${STREAM_FILE}"
fi

# ------------------------------------------------------------------------------
# 6. Comprobación y Emisión Opcional de Certificados SSL
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[+] Comprobando certificado SSL para '${PROJECT_DOMAIN}'...${NC}"
if [ ! -f "${CERT_LIVE_DIR}/${PROJECT_DOMAIN}/fullchain.pem" ]; then
    echo -e "${YELLOW}[!] AVISO: Aún no existe un certificado TLS para '${PROJECT_DOMAIN}'.${NC}"
    read -r -p "¿Deseas solicitar el certificado wildcard a Cloudflare ahora mismo? (S/n): " RUN_CERT
    if [[ ! "$RUN_CERT" =~ ^([nN][oO]|[nN])$ ]]; then
        "${REPO_ROOT}/certbot/init-cert.sh" "${PROJECT_DOMAIN}" || {
            echo -e "${RED}[!] La solicitud de certificado falló o requiere configurar cloudflare.ini.${NC}"
            echo -e "${YELLOW}[i] Podrás emitirlo luego ejecutando: ./certbot/init-cert.sh ${PROJECT_DOMAIN}${NC}"
        }
    fi
else
    echo -e "${GREEN}[✓] Certificado SSL existente detectado en certbot/conf/live/${PROJECT_DOMAIN}/${NC}"
fi

# ------------------------------------------------------------------------------
# 7. Sincronización y Recarga en Caliente
# ------------------------------------------------------------------------------
echo -e "\n${BLUE}[+] Sincronizando WireGuard y Nginx...${NC}"
"${REPO_ROOT}/scripts/reload.sh"

# ------------------------------------------------------------------------------
# 8. Obtener Clave Pública del VPS y Entregar Configuración
# ------------------------------------------------------------------------------
SERVER_PUBKEY=""
SERVER_PUB_FILE="${REPO_ROOT}/wireguard/config/server.pub"
if [ -f "${SERVER_PUB_FILE}" ]; then
    SERVER_PUBKEY=$(cat "${SERVER_PUB_FILE}" | tr -d '\r\n ')
else
    SERVER_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard cat /config/server.pub 2>/dev/null | tr -d '\r\n ' || echo "CLAVE_PUBLICA_DEL_VPS")
fi

echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN}${BOLD} ¡PROYECTO '${PROJECT_NAME}' REGISTRADO EXITOSAMENTE! ${NC}"
echo -e "${GREEN}========================================================================${NC}"
echo -e "${BOLD}Archivos generados en el VPS:${NC}"
echo -e "  - Peer WireGuard: ${PEER_FILE}"
echo -e "  - VirtualHost L7: ${NGINX_CONF_FILE}"
if [ -n "${STREAM_PORT}" ]; then
    echo -e "  - Proxy TCP L4:   ${STREAM_FILE}"
fi

echo -e "\n${BOLD}Rutas públicas habilitadas:${NC}"
echo -e "  - Web / API:  ${CYAN}https://${PROJECT_DOMAIN}${NC} y ${CYAN}https://*.${PROJECT_DOMAIN}${NC}  --> http://${PEER_IP}:80"
if [ -n "${STREAM_PORT}" ]; then
    echo -e "  - Stream TCP: ${CYAN}${VPS_ENDPOINT_HOST}:${STREAM_PORT}${NC} (TLS)                  --> ${PEER_IP}:${STREAM_INTERNAL_PORT}"
fi

echo -e "\n${YELLOW}${BOLD}------------------------------------------------------------------------${NC}"
echo -e "${YELLOW} Configuración lista para el Cliente Remoto ('/etc/wireguard/wg0.conf'): ${NC}"
echo -e "${YELLOW}${BOLD}------------------------------------------------------------------------${NC}"

cat <<EOF
[Interface]
# Clave privada generada exclusivamente para '${PROJECT_NAME}':
PrivateKey = ${CLIENT_PRIVKEY}
Address = ${PEER_IP}/16

[Peer]
# VPS Ingress Relay
PublicKey = ${SERVER_PUBKEY}
Endpoint = ${VPS_ENDPOINT_HOST}:${VPN_PORT}
AllowedIPs = 10.10.0.0/16
PersistentKeepalive = 25
EOF

echo -e "${YELLOW}------------------------------------------------------------------------${NC}"
echo -e "${CYAN}[Tip] En tu máquina local puedes usar la plantilla en 'examples/client-project/'${NC}"
echo -e "${CYAN}      para levantar WireGuard + Nginx local con esta configuración.${NC}\n"
