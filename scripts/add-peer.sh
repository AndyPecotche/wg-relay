#!/bin/bash
# ==============================================================================
# add-peer.sh: Asistente Automatizado para Registrar un Nuevo Proyecto / Peer
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
PEERS_DIR="${REPO_ROOT}/wireguard/peers.d"
ENV_FILE="${REPO_ROOT}/.env"
PEER_TEMPLATE="${REPO_ROOT}/templates/wireguard/peer.conf.template"

# Colores
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${BLUE}=== [wg-relay] Asistente de Alta de Peer / Proyecto ===${NC}"
echo -e "${YELLOW}[Tip] Para dar de alta completa (WireGuard + VirtualHost Nginx + SSL + TCP SNI), ejecuta:${NC}"
echo -e "      ${CYAN}./scripts/new-project.sh [nombre_proyecto] [dominio]${NC}\n"

# Cargar variables de entorno
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC2046
    export $(grep -v '^#' "${ENV_FILE}" | xargs)
fi

VPS_ENDPOINT_HOST="${VPS_ENDPOINT_HOST:-TU_IP_O_DOMINIO_DEL_VPS}"
VPN_PORT="${VPN_PORT:-51820}"
VPN_GATEWAY_IP="${VPN_GATEWAY_IP:-10.10.0.1}"

# 1. Obtener nombre del proyecto
PROJECT_NAME="${1:-}"
if [ -z "${PROJECT_NAME}" ]; then
    read -r -p "Nombre del proyecto/peer (ej. proyecto1, proyecto2, backend-iot): " PROJECT_NAME
fi

# Sanitizar nombre (solo minúsculas, números, guiones y guión bajo)
PROJECT_NAME=$(echo "${PROJECT_NAME}" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]_-')

if [ -z "${PROJECT_NAME}" ]; then
    echo -e "${RED}[!] ERROR: El nombre del proyecto no puede estar vacío.${NC}" >&2
    exit 1
fi

# 2. Obtener o autoasignar IP del peer en la VPN
find_next_vpn_ip() {
    local assigned_ips
    assigned_ips=$(find "${PEERS_DIR}" -type f -name "*.conf" 2>/dev/null | \
                   xargs grep -rhoE '10\.10\.[0-9]+\.[0-9]+' 2>/dev/null || true)
    local all_ips="${assigned_ips} ${VPN_GATEWAY_IP}"

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

PEER_IP="${2:-}"
if [ -z "${PEER_IP}" ]; then
    AUTO_IP=$(find_next_vpn_ip)
    echo -e "${GREEN}[+] Siguiente IP disponible:${NC} ${AUTO_IP}"
    read -r -p "Presiona Enter para usar ${AUTO_IP} o ingresa otra IP: " USER_IP
    PEER_IP="${USER_IP:-${AUTO_IP}}"
fi

# Limpiar posible máscara si el usuario escribió /32 o /16
PEER_IP="${PEER_IP%/*}"

PEER_FILE="${PEERS_DIR}/${PEER_IP}-${PROJECT_NAME}.conf"

if [ -f "${PEER_FILE}" ]; then
    echo -e "${RED}[!] ERROR: Ya existe una configuración para el proyecto '${PROJECT_NAME}' en ${PEER_FILE}.${NC}" >&2
    exit 1
fi

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

mkdir -p "${PEERS_DIR}"
if [ -f "${PEER_TEMPLATE}" ]; then
    sed -e "s|{{PROJECT_NAME}}|${PROJECT_NAME}|g" \
        -e "s|{{PROJECT_DOMAIN}}|${PROJECT_NAME}.local|g" \
        -e "s|{{CREATION_DATE}}|$(date -u +"%Y-%m-%dT%H:%M:%SZ")|g" \
        -e "s|{{CLIENT_PUBKEY}}|${CLIENT_PUBKEY}|g" \
        -e "s|{{PEER_IP}}|${PEER_IP}|g" \
        "${PEER_TEMPLATE}" > "${PEER_FILE}"
else
    cat <<EOF > "${PEER_FILE}"
[Peer]
# Proyecto: ${PROJECT_NAME}
# Fecha de creación: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
PublicKey = ${CLIENT_PUBKEY}
AllowedIPs = ${PEER_IP}/32
EOF
fi

chmod 600 "${PEER_FILE}"
echo -e "${GREEN}[✓] Archivo de peer creado: ${PEER_FILE}${NC}"

# 5. Obtener clave pública del servidor Relay
SERVER_PUBKEY=""
SERVER_PUB_FILE="${REPO_ROOT}/wireguard/config/server.pub"

if docker compose -f "${REPO_ROOT}/docker-compose.yml" ps --status running --format json 2>/dev/null | grep -q "wireguard"; then
    SERVER_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard wg show wg0 public-key 2>/dev/null | tr -d '\r\n ' || true)
fi

if [ -z "${SERVER_PUBKEY}" ]; then
    if [ -f "${SERVER_PUB_FILE}" ]; then
        SERVER_PUBKEY=$(cat "${SERVER_PUB_FILE}" 2>/dev/null | tr -d '\r\n ' || true)
    fi
fi

if [ -z "${SERVER_PUBKEY}" ]; then
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
Endpoint = ${VPS_ENDPOINT_HOST}:${VPN_PORT}
AllowedIPs = 10.10.0.0/16
PersistentKeepalive = 25
EOF

echo -e "${GREEN}========================================================================${NC}"
echo -e "${CYAN}[Próximo paso] Para exponer este servicio públicamente en Nginx:${NC}"
echo -e "  - L7 Web/API: Crea 'nginx/conf.d/${PROJECT_NAME}.conf' apuntando a http://${PEER_IP}:<puerto>"
echo -e "  - L4 TCP/MQTTS: Crea 'nginx/stream.d/${PROJECT_NAME}.conf' apuntando a ${PEER_IP}:<puerto>"
echo -e "  - Luego ejecuta: ./scripts/reload.sh\n"

