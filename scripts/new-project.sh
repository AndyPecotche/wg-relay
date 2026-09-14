#!/bin/bash
# ==============================================================================
# new-project.sh: Asistente Integral para Alta Automatizada de Proyectos
# ==============================================================================
# Automatiza:
# 1. Asignación automática de IP en la VPN (10.10.x.y)
# 2. Generación de claves WireGuard y archivo en wireguard/peers.d/<IP>-<proyecto>.conf
# 3. Creación de VirtualHost Nginx L7 en nginx/conf.d/<IP>-<dominio>.conf
# 4. Configuración opcional de TCP Stream con TLS en puerto 443 (SNI Multiplexer)
#    generando nginx/stream.d/<IP>-<dominio>.map y .conf con terminador interno (10001+)
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
# Funciones Auxiliares
# ------------------------------------------------------------------------------
get_server_pubkey() {
    local key=""
    local pub_file="${REPO_ROOT}/wireguard/config/server.pub"
    if docker compose -f "${REPO_ROOT}/docker-compose.yml" ps --status running --format json 2>/dev/null | grep -q "wireguard"; then
        key=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard wg show wg0 public-key 2>/dev/null | tr -d '\r\n ' || true)
    fi
    if [ -z "${key}" ] && [ -f "${pub_file}" ]; then
        key=$(cat "${pub_file}" 2>/dev/null | tr -d '\r\n ' || true)
    fi
    if [ -z "${key}" ]; then
        key=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard cat /config/server.pub 2>/dev/null | tr -d '\r\n ' || echo "CLAVE_PUBLICA_DEL_VPS")
    fi
    echo "${key}"
}

check_cert_exists() {
    local domain="$1"
    if [ -f "${CERT_LIVE_DIR}/${domain}/fullchain.pem" ]; then
        return 0
    fi
    # Comprobar dentro del contenedor si en el host falla por permisos de root (0700)
    if docker compose -f "${REPO_ROOT}/docker-compose.yml" ps --services --filter "status=running" 2>/dev/null | grep -q "^certbot$"; then
        docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T certbot /bin/sh -c "test -f /etc/letsencrypt/live/${domain}/fullchain.pem" 2>/dev/null && return 0
    else
        docker compose -f "${REPO_ROOT}/docker-compose.yml" run --rm --entrypoint /bin/sh certbot -c "test -f /etc/letsencrypt/live/${domain}/fullchain.pem" 2>/dev/null && return 0
    fi
    return 1
}

find_next_vpn_ip() {
    # Recolectar todas las IPs ya asignadas en peers.d, conf.d y stream.d (incluyendo .disabled)
    local assigned_ips
    assigned_ips=$(find "${PEERS_DIR}" "${CONF_DIR}" "${STREAM_DIR}" -type f \( -name "*.conf" -o -name "*.conf.disabled" -o -name "*.map" -o -name "*.map.disabled" \) 2>/dev/null | \
                   xargs grep -rhoE '10\.10\.[0-9]+\.[0-9]+' 2>/dev/null || true)
    local file_ips
    file_ips=$(find "${PEERS_DIR}" "${CONF_DIR}" "${STREAM_DIR}" -type f -name "10.10.*" 2>/dev/null | \
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

# ------------------------------------------------------------------------------
# 2. Comprobación de Existencia de Proyecto / Dominio Previo
# ------------------------------------------------------------------------------
EXISTING_PEER_FILE=$(find "${PEERS_DIR}" -type f \( -name "*-${PROJECT_NAME}.conf" -o -name "*-${PROJECT_NAME}.conf.disabled" \) 2>/dev/null | head -n 1 || true)
EXISTING_CONF_FILE=$(find "${CONF_DIR}" -type f \( -name "*-${PROJECT_DOMAIN}.conf" -o -name "*-${PROJECT_DOMAIN}.conf.disabled" \) 2>/dev/null | head -n 1 || true)
EXISTING_STREAM_MAP=$(find "${STREAM_DIR}" -type f \( -name "*-${PROJECT_DOMAIN}.map" -o -name "*-${PROJECT_DOMAIN}.map.disabled" \) 2>/dev/null | head -n 1 || true)
EXISTING_STREAM_CONF=$(find "${STREAM_DIR}" -type f \( -name "*-${PROJECT_DOMAIN}.conf" -o -name "*-${PROJECT_DOMAIN}.conf.disabled" \) 2>/dev/null | head -n 1 || true)

ANY_EXISTING="${EXISTING_PEER_FILE:-${EXISTING_CONF_FILE:-${EXISTING_STREAM_MAP:-$EXISTING_STREAM_CONF}}}"
PRESET_IP=""

if [ -n "${ANY_EXISTING}" ]; then
    EXISTING_IP=$(echo "${ANY_EXISTING}" | grep -oE '10\.10\.[0-9]+\.[0-9]+' | head -n 1 || true)
    PRESET_IP="${EXISTING_IP}"
    
    # Recolectar archivos .disabled asociados a este proyecto/dominio
    DISABLED_FILES=()
    for df in "${CONF_DIR}/${EXISTING_IP}-${PROJECT_DOMAIN}.conf.disabled" \
              "${STREAM_DIR}/${EXISTING_IP}-${PROJECT_DOMAIN}.map.disabled" \
              "${STREAM_DIR}/${EXISTING_IP}-${PROJECT_DOMAIN}.conf.disabled" \
              "${PEERS_DIR}/${EXISTING_IP}-${PROJECT_NAME}.conf.disabled"; do
        [ -f "${df}" ] && DISABLED_FILES+=("${df}")
    done

    if [ ${#DISABLED_FILES[@]} -gt 0 ]; then
        echo -e "\n${YELLOW}[!] AVISO: El proyecto '${PROJECT_NAME}' ya existe asignado a la IP ${BOLD}${EXISTING_IP}${NC}${YELLOW},"
        echo -e "    pero cuenta con archivos desactivados (.disabled):${NC}"
        for df in "${DISABLED_FILES[@]}"; do
            echo -e "    - $(basename "${df}")"
        done

        echo -e "\n${CYAN}[+] Verificando certificado SSL para '${PROJECT_DOMAIN}'...${NC}"
        if ! check_cert_exists "${PROJECT_DOMAIN}"; then
            echo -e "${YELLOW}[!] Aún no existe certificado TLS para '${PROJECT_DOMAIN}'.${NC}"
            read -r -p "¿Deseas solicitar el certificado wildcard a Cloudflare ahora? (S/n): " RUN_CERT
            if [[ ! "$RUN_CERT" =~ ^([nN][oO]|[nN])$ ]]; then
                "${REPO_ROOT}/certbot/init-cert.sh" "${PROJECT_DOMAIN}" || true
            fi
        fi

        if check_cert_exists "${PROJECT_DOMAIN}"; then
            echo -e "${GREEN}[✓] Certificado SSL verificado en certbot/conf/live/${PROJECT_DOMAIN}/${NC}"
            read -r -p "¿Deseas habilitar y activar los archivos .disabled ahora mismo? (S/n): " ENABLE_FILES
            if [[ ! "$ENABLE_FILES" =~ ^([nN][oO]|[nN])$ ]]; then
                for df in "${DISABLED_FILES[@]}"; do
                    target="${df%.disabled}"
                    mv "${df}" "${target}"
                    echo -e "  ${GREEN}[✓] Activado:${NC} $(basename "${target}")"
                done
                echo -e "\n${BLUE}[+] Sincronizando y recargando servicios...${NC}"
                "${REPO_ROOT}/scripts/reload.sh"
                
                SERVER_PUBKEY=$(get_server_pubkey)
                echo -e "\n${GREEN}========================================================================${NC}"
                echo -e "${GREEN}${BOLD} ¡PROYECTO '${PROJECT_NAME}' HABILITADO Y ACTIVO CON ÉXITO! ${NC}"
                echo -e "${GREEN}========================================================================${NC}"
                echo -e "IP VPN: ${EXISTING_IP}"
                echo -e "Web:    https://${PROJECT_DOMAIN} y https://*.${PROJECT_DOMAIN} -> http://${EXISTING_IP}:80"
                if [ -f "${STREAM_DIR}/${EXISTING_IP}-${PROJECT_DOMAIN}.map" ]; then
                    echo -e "TCP L4: https://*.${PROJECT_DOMAIN}:443 (SNI) -> ${EXISTING_IP}"
                fi
                echo -e "\n${YELLOW}Clave pública del Relay VPS: ${SERVER_PUBKEY}${NC}\n"
                exit 0
            fi
        else
            echo -e "${RED}[!] El certificado SSL aún no está listo. Los archivos permanecen como .disabled.${NC}"
            exit 1
        fi
    else
        # El proyecto ya está completamente activo
        echo -e "\n${YELLOW}[!] AVISO: El proyecto '${PROJECT_NAME}' (dominio '${PROJECT_DOMAIN}') ya se encuentra"
        echo -e "    registrado y activo con la IP ${BOLD}${EXISTING_IP}${NC}${YELLOW}.${NC}"
        read -r -p "¿Deseas ver la configuración activa para el cliente WireGuard? (S/n): " SHOW_CFG
        if [[ ! "$SHOW_CFG" =~ ^([nN][oO]|[nN])$ ]]; then
            SERVER_PUBKEY=$(get_server_pubkey)
            echo -e "\n${YELLOW}------------------------------------------------------------------------${NC}"
            echo -e "${YELLOW} Configuración del Cliente Remoto: ${NC}"
            echo -e "${YELLOW}------------------------------------------------------------------------${NC}"
            echo -e "[Interface]"
            echo -e "Address = ${EXISTING_IP}/16"
            echo -e ""
            echo -e "[Peer]"
            echo -e "PublicKey = ${SERVER_PUBKEY}"
            echo -e "Endpoint = ${VPS_ENDPOINT_HOST}:${VPN_PORT}"
            echo -e "AllowedIPs = 10.10.0.0/16"
            echo -e "PersistentKeepalive = 25"
            echo -e "${YELLOW}------------------------------------------------------------------------${NC}\n"
        fi
        read -r -p "¿Deseas sobreescribir / reconfigurar este proyecto? (s/N): " RECONF
        if [[ ! "$RECONF" =~ ^([sS][iI]|[sS])$ ]]; then
            echo "Operación cancelada."
            exit 0
        fi
    fi
fi

# ------------------------------------------------------------------------------
# 3. Algoritmo de Asignación de IP VPN (10.10.x.y)
# ------------------------------------------------------------------------------
if [ -n "${PRESET_IP}" ]; then
    AUTO_IP="${PRESET_IP}"
else
    AUTO_IP=$(find_next_vpn_ip)
fi

echo -e "${GREEN}[+] IP disponible detectada:${NC} ${BOLD}${AUTO_IP}${NC}"
read -r -p "Presiona Enter para usar ${AUTO_IP} o ingresa una IP diferente: " CUSTOM_IP
PEER_IP="${CUSTOM_IP:-${AUTO_IP}}"
PEER_IP="${PEER_IP%/*}"

# Validar formato
if [[ ! "${PEER_IP}" =~ ^10\.10\.[0-9]{1,3}\.[0-9]{1,3}$ ]]; then
    echo -e "${YELLOW}[!] AVISO: '${PEER_IP}' no sigue el formato habitual 10.10.x.y.${NC}"
fi

# ------------------------------------------------------------------------------
# 4. Generación de Claves WireGuard y Archivo de Peer
# ------------------------------------------------------------------------------
mkdir -p "${PEERS_DIR}"
PEER_FILE="${PEERS_DIR}/${PEER_IP}-${PROJECT_NAME}.conf"

# Limpiar archivos huérfanos con IPs distintas para el mismo proyecto
for d in "${PEERS_DIR}" "${CONF_DIR}" "${STREAM_DIR}"; do
    for old_file in "${d}"/*"-${PROJECT_NAME}."* "${d}"/*"-${PROJECT_DOMAIN}."*; do
        [ -e "${old_file}" ] || continue
        case "$(basename "${old_file}")" in
            "${PEER_IP}-"*) continue ;; # Conservar el actual
            *)
                echo -e "${YELLOW}[i] Eliminando archivo huérfano de configuración anterior: $(basename "${old_file}")${NC}"
                rm -f "${old_file}"
                ;;
        esac
    done
done

CLIENT_PUBKEY=""
CLIENT_PRIVKEY=""

if [ -f "${PEER_FILE}" ]; then
    EXISTING_CLIENT_PUBKEY=$(grep -E '^[[:space:]]*PublicKey' "${PEER_FILE}" | head -n1 | awk '{print $NF}' || true)
    if [ -n "${EXISTING_CLIENT_PUBKEY}" ]; then
        echo -e "${YELLOW}[i] Clave pública de cliente detectada en el peer actual: ${EXISTING_CLIENT_PUBKEY}${NC}"
        read -r -p "¿Deseas conservar la clave pública actual del cliente? (S/n): " KEEP_CLIENT_KEY
        if [[ ! "$KEEP_CLIENT_KEY" =~ ^([nN][oO]|[nN])$ ]]; then
            CLIENT_PUBKEY="${EXISTING_CLIENT_PUBKEY}"
            echo -e "${GREEN}[✓] Conservando clave actual del cliente (no necesitarás reconfigurar el cliente).${NC}"
        fi
    fi
fi

if [ -z "${CLIENT_PUBKEY}" ]; then
    echo -e "${CYAN}[+] Generando nuevo par de claves WireGuard para el cliente...${NC}"
    if command -v wg >/dev/null 2>&1; then
        CLIENT_PRIVKEY=$(wg genkey)
        CLIENT_PUBKEY=$(echo "${CLIENT_PRIVKEY}" | wg pubkey)
    else
        CLIENT_PRIVKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard wg genkey | tr -d '\r\n')
        CLIENT_PUBKEY=$(docker compose -f "${REPO_ROOT}/docker-compose.yml" exec -T wireguard /bin/sh -c "echo '${CLIENT_PRIVKEY}' | wg pubkey" | tr -d '\r\n')
    fi
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
# 5. Comprobación y Emisión de Certificados SSL (Antes de Nginx)
# ------------------------------------------------------------------------------
echo -e "\n${CYAN}[+] Comprobando certificado SSL para '${PROJECT_DOMAIN}'...${NC}"
if ! check_cert_exists "${PROJECT_DOMAIN}"; then
    echo -e "${YELLOW}[!] AVISO: Aún no existe un certificado TLS para '${PROJECT_DOMAIN}'.${NC}"
    read -r -p "¿Deseas solicitar el certificado wildcard a Cloudflare ahora mismo? (S/n): " RUN_CERT
    if [[ ! "$RUN_CERT" =~ ^([nN][oO]|[nN])$ ]]; then
        "${REPO_ROOT}/certbot/init-cert.sh" "${PROJECT_DOMAIN}" || {
            echo -e "${RED}[!] La solicitud de certificado no pudo completarse en este momento.${NC}"
        }
    fi
fi

HAS_SSL_CERT=false
FILE_EXT=".conf"
MAP_EXT=".map"
if check_cert_exists "${PROJECT_DOMAIN}"; then
    HAS_SSL_CERT=true
    echo -e "${GREEN}[✓] Certificado SSL activo verificado en certbot/conf/live/${PROJECT_DOMAIN}/${NC}"
    # Si existían archivos .disabled anteriores de un intento previo, activarlos o limpiarlos
    rm -f "${CONF_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf.disabled" \
          "${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.map.disabled" \
          "${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf.disabled" 2>/dev/null || true
else
    echo -e "\n${YELLOW}[!] ADVERTENCIA: Certificado TLS no encontrado para '${PROJECT_DOMAIN}'.${NC}"
    echo -e "${YELLOW}[i] Para proteger Nginx contra errores de sintaxis, los archivos se generarán como '.disabled'.${NC}"
    echo -e "${YELLOW}[i] Podrás emitir el certificado luego con: ./certbot/init-cert.sh ${PROJECT_DOMAIN}${NC}"
    FILE_EXT=".conf.disabled"
    MAP_EXT=".map.disabled"
fi

# ------------------------------------------------------------------------------
# 5. Creación de VirtualHost Nginx Layer 7 (conf.d)
# ------------------------------------------------------------------------------
mkdir -p "${CONF_DIR}"
NGINX_CONF_FILE="${CONF_DIR}/${PEER_IP}-${PROJECT_DOMAIN}${FILE_EXT}"

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
    listen 127.0.0.1:8443 ssl;
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
# 6. Configuración Opcional de TCP Stream con TLS en Puerto 443 (SNI Multiplexer)
# ------------------------------------------------------------------------------
find_next_internal_stream_port() {
    local config_ports
    config_ports=$(find "${STREAM_DIR}" -type f -name "*.conf" 2>/dev/null | \
                   xargs grep -rhoE '127\.0\.0\.1:[0-9]+' 2>/dev/null | sed 's/127\.0\.0\.1://' || true)
    local map_ports
    map_ports=$(find "${STREAM_DIR}" -type f -name "*.map" 2>/dev/null | \
                xargs grep -rhoE '127\.0\.0\.1:[0-9]+' 2>/dev/null | sed 's/127\.0\.0\.1://' || true)
    local all_ports="${config_ports} ${map_ports} 8443"

    for port in $(seq 10001 20000); do
        if ! echo "${all_ports}" | grep -qw "${port}"; then
            echo "${port}"
            return 0
        fi
    done
    return 1
}

STREAM_INTERNAL_PORT=""
STREAM_REMOTE_PORT=""
TCP_SUBDOMAIN=""
STREAM_CONF_FILE=""
STREAM_MAP_FILE=""

echo -e "\n${BOLD}¿Deseas configurar un servicio TCP con TLS en puerto 443 (SNI Multiplexer)?${NC} (ej. MQTTS, gRPC, DB, sockets)"
read -r -p "Habilitar proxy TCP SNI en puerto 443? (s/N): " ENABLE_STREAM

if [[ "$ENABLE_STREAM" =~ ^([sS][iI]|[sS])$ ]]; then
    mkdir -p "${STREAM_DIR}"

    # Asignar puerto loopback interno
    STREAM_INTERNAL_PORT=$(find_next_internal_stream_port)

    # Preguntar subdominio TCP para multiplexar por SNI
    DEFAULT_TCP_SUBDOMAIN="tcp.${PROJECT_DOMAIN}"
    read -r -p "Subdominio TLS para este servicio TCP [default: ${DEFAULT_TCP_SUBDOMAIN}]: " USER_SUBDOMAIN
    TCP_SUBDOMAIN="${USER_SUBDOMAIN:-${DEFAULT_TCP_SUBDOMAIN}}"
    TCP_SUBDOMAIN=$(echo "${TCP_SUBDOMAIN}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')

    # Preguntar puerto TCP destino en el cliente remoto
    read -r -p "Puerto TCP interno en el cliente remoto (ej. 1883 para MQTT, 5432 para DB) [default: 1883]: " USER_REM_PORT
    STREAM_REMOTE_PORT="${USER_REM_PORT:-1883}"

    STREAM_MAP_FILE="${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}${MAP_EXT}"
    STREAM_CONF_FILE="${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}${FILE_EXT}"

    # Evitar duplicados de la misma regla SNI en stream.d
    for existing_map in "${STREAM_DIR}"/*.map*; do
        [ -e "${existing_map}" ] || continue
        [ "${existing_map}" = "${STREAM_MAP_FILE}" ] && continue
        if grep -qE "^[[:space:]]*${TCP_SUBDOMAIN}[[:space:]]+" "${existing_map}" 2>/dev/null; then
            echo -e "${YELLOW}[i] Eliminando regla SNI previa en conflicto para '${TCP_SUBDOMAIN}' en $(basename "${existing_map}")${NC}"
            rm -f "${existing_map}" "${existing_map%.map*}.conf" "${existing_map%.map*}.conf.disabled" 2>/dev/null || true
        fi
    done

    # Generar mapeo SNI
    cat <<EOF > "${STREAM_MAP_FILE}"
# ==============================================================================
# Regla SNI (Port 443 Stream Multiplexer): ${PROJECT_NAME}
# ==============================================================================
${TCP_SUBDOMAIN}    127.0.0.1:${STREAM_INTERNAL_PORT};
EOF

    # Generar terminador TLS interno
    cat <<EOF > "${STREAM_CONF_FILE}"
# ==============================================================================
# Terminador TLS Interno (Stream L4): ${PROJECT_NAME} (${TCP_SUBDOMAIN})
# Escucha interna: 127.0.0.1:${STREAM_INTERNAL_PORT} -> Destino remoto: ${PEER_IP}:${STREAM_REMOTE_PORT}
# ==============================================================================

server {
    listen 127.0.0.1:${STREAM_INTERNAL_PORT} ssl;

    ssl_certificate     /etc/letsencrypt/live/${PROJECT_DOMAIN}/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/${PROJECT_DOMAIN}/privkey.pem;

    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers HIGH:!aNULL:!MD5;
    ssl_session_cache shared:SSL_TCP_${PROJECT_NAME}_${STREAM_INTERNAL_PORT}:10m;
    ssl_session_timeout 4h;

    proxy_timeout 1h;
    proxy_connect_timeout 10s;

    proxy_pass ${PEER_IP}:${STREAM_REMOTE_PORT};
}
EOF
    echo -e "${GREEN}[✓] Regla SNI creada:${NC} ${STREAM_MAP_FILE}"
    echo -e "${GREEN}[✓] Terminador TLS interno creado (puerto loopback 127.0.0.1:${STREAM_INTERNAL_PORT}):${NC} ${STREAM_CONF_FILE}"
fi

# ------------------------------------------------------------------------------
# 7. Sincronización y Recarga en Caliente
# ------------------------------------------------------------------------------
echo -e "\n${BLUE}[+] Sincronizando servicios...${NC}"
if [ "${HAS_SSL_CERT}" = true ]; then
    "${REPO_ROOT}/scripts/reload.sh"
else
    "${REPO_ROOT}/scripts/reload.sh" --wg-only
    echo -e "${YELLOW}[i] Solo se sincronizó WireGuard. Cuando emitas el certificado para '${PROJECT_DOMAIN}', ejecuta:${NC}"
    echo -e "${CYAN}    mv ${CONF_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf.disabled ${CONF_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf${NC}"
    if [ -n "${STREAM_CONF_FILE}" ]; then
        echo -e "${CYAN}    mv ${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.map.disabled ${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.map${NC}"
        echo -e "${CYAN}    mv ${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf.disabled ${STREAM_DIR}/${PEER_IP}-${PROJECT_DOMAIN}.conf${NC}"
    fi
    echo -e "${CYAN}    ./scripts/reload.sh${NC}"
fi

# ------------------------------------------------------------------------------
# 8. Obtener Clave Pública del VPS y Entregar Configuración
# ------------------------------------------------------------------------------
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

echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN}${BOLD} ¡PROYECTO '${PROJECT_NAME}' REGISTRADO EXITOSAMENTE! ${NC}"
echo -e "${GREEN}========================================================================${NC}"
echo -e "${BOLD}Archivos generados en el VPS:${NC}"
echo -e "  - Peer WireGuard:  ${PEER_FILE}"
echo -e "  - VirtualHost L7:  ${NGINX_CONF_FILE}"
if [ -n "${STREAM_CONF_FILE}" ]; then
    echo -e "  - Regla SNI L4:    ${STREAM_MAP_FILE}"
    echo -e "  - Terminador L4:   ${STREAM_CONF_FILE} (Loopback interno: 127.0.0.1:${STREAM_INTERNAL_PORT})"
fi

echo -e "\n${BOLD}Rutas públicas habilitadas (¡Todas unificadas en puerto 80 / 443!):${NC}"
echo -e "  - Web / API:   ${CYAN}https://${PROJECT_DOMAIN}${NC} y ${CYAN}https://*.${PROJECT_DOMAIN}${NC}  --> http://${PEER_IP}:80"
if [ -n "${STREAM_CONF_FILE}" ]; then
    echo -e "  - TCP con TLS: ${CYAN}${TCP_SUBDOMAIN}:443${NC} (MQTTS/gRPC/etc. con SNI)  --> ${PEER_IP}:${STREAM_REMOTE_PORT}"
fi

echo -e "\n${YELLOW}${BOLD}------------------------------------------------------------------------${NC}"
echo -e "${YELLOW} Configuración lista para el Cliente Remoto ('/etc/wireguard/wg0.conf'): ${NC}"
echo -e "${YELLOW}${BOLD}------------------------------------------------------------------------${NC}"

PRIVKEY_BLOCK=""
if [ -n "${CLIENT_PRIVKEY}" ]; then
    PRIVKEY_BLOCK="# Clave privada generada exclusivamente para '${PROJECT_NAME}':
PrivateKey = ${CLIENT_PRIVKEY}"
else
    PRIVKEY_BLOCK="# Conserva la clave privada configurada en tu cliente local:
PrivateKey = <TU_CLAVE_PRIVADA_LOCAL>"
fi

cat <<EOF
[Interface]
${PRIVKEY_BLOCK}
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

