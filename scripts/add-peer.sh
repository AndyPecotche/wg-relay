#!/bin/bash
# ==============================================================================
# add-peer.sh: Asistente Automatizado para Registrar un Nuevo Proyecto / Peer
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PEERS_DIR="${REPO_ROOT}/wireguard/peers.d"
ENV_FILE="${REPO_ROOT}/.env"

# Colores
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${BLUE}=== [wg-relay] Asistente de Alta de Peer / Proyecto ===${NC}"

# Cargar variables de entorno
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC2046
    export $(grep -v '^#' "${ENV_FILE}" | xargs)
fi

BASE_DOMAIN="${BASE_DOMAIN:-vps.tudominio.com}"
VPN_PORT="${VPN_PORT:-51820}"
VPN_GATEWAY_IP="${VPN_GATEWAY_IP:-10.10.0.1}"

# 1. Obtener nombre del proyecto
PROJECT_NAME="${1:-}"
if [ -z "${PROJECT_NAME}" ]; then
    read -r -p "Nombre del proyecto/peer (ej. sensorhub, telemetry, iot-gateway): " PROJECT_NAME
fi

# Sanitizar nombre (solo minúsculas, números, guiones y guión bajo)
PROJECT_NAME=$(echo "${PROJECT_NAME}" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]_-')

if [ -z "${PROJECT_NAME}" ]; then
    echo -e "${RED}[!] ERROR: El nombre del proyecto no puede estar vacío.${NC}" >&2
    exit 1
fi

PEER_FILE="${PEERS_DIR}/${PROJECT_NAME}.conf"

if [ -f "${PEER_FILE}" ]; then
    echo -e "${RED}[!] ERROR: Ya existe una configuración para el proyecto '${PROJECT_NAME}' en ${PEER_FILE}.${NC}" >&2
    exit 1
fi

# 2. Obtener IP del peer en la VPN
PEER_IP="${2:-}"
if [ -z "${PEER_IP}" ]; then
    read -r -p "Dirección IP asignada dentro de la VPN (ej. 10.10.1.2): " PEER_IP
fi

# Limpiar posible máscara si el usuario escribió /32 o /16
PEER_IP="${PEER_IP%/*}"

# Validar formato básico de IP
if [[ ! "${PEER_IP}" =~ ^10\.10\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    echo -e "${YELLOW}[!] AVISO: '${PEER_IP}' no pertenece a la subred estándar 10.10.0.0/16.${NC}"
fi

# Validar colisión de IP con peers existentes
if grep -rn "AllowedIPs = ${PEER_IP}" "${PEERS_DIR}"/*.conf 2>/dev/null; then
    echo -e "${RED}[!] ERROR: La IP ${PEER_IP} ya está asignada a otro peer.${NC}" >&2
    exit 1
fi

# 3. Obtener o generar claves
CLIENT_PUBKEY="${3:-}"
CLIENT_PRIVKEY=""

if [ -z "${CLIENT_PUBKEY}" ]; then
    echo -e "${CYAN}[+] No se especificó clave pública. Generando nuevo par de claves cliente...${NC}"
    if command -v wg >/dev/null 2>&1; then
        CLIENT_PRIVKEY=$(wg genkey)
        CLIENT_PUBKEY=$(echo "${CLIENT_PRIVKEY}" | wg pubkey)
    else
        # Generar dentro del contenedor Docker WireGuard si wg no está en el host
        CLIENT_PRIVKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard wg genkey | tr -d '\r\n')
        CLIENT_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard /bin/sh -c "echo '${CLIENT_PRIVKEY}' | wg pubkey" | tr -d '\r\n')
    fi
    echo -e "${GREEN}[✓] Claves generadas exitosamente.${NC}"
fi

# 4. Crear archivo modular del peer en peers.d/
mkdir -p "${PEERS_DIR}"
cat <<EOF > "${PEER_FILE}"
[Peer]
# Proyecto: ${PROJECT_NAME}
# Fecha de creación: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
PublicKey = ${CLIENT_PUBKEY}
AllowedIPs = ${PEER_IP}/32
EOF

chmod 600 "${PEER_FILE}"
echo -e "${GREEN}[✓] Archivo de peer creado: ${PEER_FILE}${NC}"

# 5. Obtener clave pública del servidor Relay
SERVER_PUBKEY=""
SERVER_PUB_FILE="${REPO_ROOT}/wireguard/config/server.pub"
if [ -f "${SERVER_PUB_FILE}" ]; then
    SERVER_PUBKEY=$(cat "${SERVER_PUB_FILE}" | tr -d '\r\n ')
else
    # Si el archivo aún no existe en host, intentar consultar al contenedor
    SERVER_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard cat /config/server.pub 2>/dev/null | tr -d '\r\n ' || echo "CLAVE_PUBLICA_DEL_VPS")
fi

# 6. Recargar WireGuard en caliente
echo -e "${BLUE}[+] Aplicando sincronización en caliente...${NC}"
"${REPO_ROOT}/scripts/reload.sh" --wg-only

# 7. Imprimir configuración para el cliente remoto (Edge Device / Gateway)
echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN} Configuración del Cliente WireGuard (${PROJECT_NAME}) ${NC}"
echo -e "${YELLOW} Copia este contenido en '/etc/wireguard/wg0.conf' del nodo remoto:${NC}"
echo -e "${GREEN}========================================================================${NC}"

cat <<EOF
[Interface]
${CLIENT_PRIVKEY:+# Clave privada autogenerada (GUARDAR EN SECRETO):}
${CLIENT_PRIVKEY:+PrivateKey = ${CLIENT_PRIVKEY}}
${CLIENT_PRIVKEY:-PrivateKey = <CLAVE_PRIVADA_DEL_CLIENTE>}
Address = ${PEER_IP}/16

[Peer]
# VPS Ingress Relay
PublicKey = ${SERVER_PUBKEY}
Endpoint = ${BASE_DOMAIN}:${VPN_PORT}
AllowedIPs = 10.10.0.0/16
PersistentKeepalive = 25
EOF

echo -e "${GREEN}========================================================================${NC}"
echo -e "${CYAN}[Próximo paso] Para exponer este servicio públicamente en Nginx:${NC}"
echo -e "  - L7 Web/API: Crea 'nginx/conf.d/${PROJECT_NAME}.conf' apuntando a http://${PEER_IP}:<puerto>"
echo -e "  - L4 TCP/MQTTS: Crea 'nginx/stream.d/${PROJECT_NAME}.conf' apuntando a ${PEER_IP}:<puerto>"
echo -e "  - Luego ejecuta: ./scripts/reload.sh\n"

