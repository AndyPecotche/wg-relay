#!/bin/bash
# ==============================================================================
# init-cert.sh: Emisión de Certificados Wildcard DNS-01 con Cloudflare
# ==============================================================================
# Soporta emisión de certificados independientes para cualquier dominio.
# Uso: ./init-cert.sh [dominio] [archivo_cloudflare.ini_opcional]
# Ejemplo: ./init-cert.sh proyecto1.com
#          ./init-cert.sh cliente-externo.org certbot/cloudflare-cliente.ini
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

ENV_FILE="${REPO_ROOT}/.env"
CF_EXAMPLE="${SCRIPT_DIR}/cloudflare.ini.example"

# Colores para la terminal
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

echo -e "${BLUE}=== [Certbot DNS-01] Emisión de Certificados Wildcard ===${NC}"

# 1. Cargar email desde .env si existe
CERTBOT_EMAIL="admin@example.com"
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC2046
    export $(grep -v '^#' "${ENV_FILE}" | xargs)
fi

# 2. Obtener dominio objetivo (por argumento o interactivo)
TARGET_DOMAIN="${1:-}"
if [ -z "${TARGET_DOMAIN}" ]; then
    read -r -p "Ingresa el dominio base a certificar (ej. proyecto1.com): " TARGET_DOMAIN
fi

# Sanitizar dominio
TARGET_DOMAIN=$(echo "${TARGET_DOMAIN}" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')
TARGET_DOMAIN="${TARGET_DOMAIN#\*.}" # Si el usuario escribió *.dominio.com, quitar *.

if [ -z "${TARGET_DOMAIN}" ]; then
    echo -e "${RED}[!] ERROR: El nombre de dominio no puede estar vacío.${NC}" >&2
    exit 1
fi

# 3. Determinar archivo de credenciales de Cloudflare
CUSTOM_CF_INI="${2:-${SCRIPT_DIR}/cloudflare.ini}"
if [ ! -f "${CUSTOM_CF_INI}" ]; then
    echo -e "${RED}[!] ERROR: No se encuentra el archivo de credenciales ${CUSTOM_CF_INI}.${NC}"
    echo -e "${YELLOW}[i] Copia ${CF_EXAMPLE} a ${CUSTOM_CF_INI} y define tu API Token de Cloudflare.${NC}"
    exit 1
fi

if grep -q "0123456789abcdef0123456789abcdef01234567" "${CUSTOM_CF_INI}"; then
    echo -e "${RED}[!] ERROR: Debes colocar tu token real de Cloudflare en ${CUSTOM_CF_INI}.${NC}"
    exit 1
fi

# Asegurar permisos estrictos (chmod 600)
chmod 600 "${CUSTOM_CF_INI}"
echo -e "${GREEN}[✓] Permisos en credenciales ajustados a 600.${NC}"

# 4. Verificar si ya existe certificado emitido para este dominio
if [ -d "${SCRIPT_DIR}/conf/live/${TARGET_DOMAIN}" ]; then
    echo -e "${YELLOW}[!] AVISO: Ya existe un certificado para '${TARGET_DOMAIN}' en certbot/conf/live/${TARGET_DOMAIN}.${NC}"
    read -r -p "¿Deseas forzar la re-emisión? (s/N): " response
    if [[ ! "$response" =~ ^([sS][iI]|[sS])$ ]]; then
        echo "Operación cancelada."
        exit 0
    fi
fi

# 5. Ejecutar Certbot vía Docker Compose
echo -e "${BLUE}[+] Solicitando certificado para '${TARGET_DOMAIN}' y '*.${TARGET_DOMAIN}' mediante DNS-01...${NC}"

# Ruta relativa del archivo ini dentro del volumen /etc/letsencrypt
CF_MOUNT_PATH="/etc/letsencrypt/cloudflare.ini"

cd "${REPO_ROOT}"
if docker compose ps --services --filter "status=running" 2>/dev/null | grep -q "^certbot$"; then
    # Esperar si certbot ya está ejecutando una renovación en segundo plano
    while docker compose exec -T certbot pgrep -f "certbot" >/dev/null 2>&1; do
        echo -e "${YELLOW}[i] Certbot está ocupado con una tarea en segundo plano. Esperando unos segundos...${NC}"
        sleep 3
    done
    docker compose exec -T certbot certbot certonly \
        --dns-cloudflare \
        --dns-cloudflare-credentials "${CF_MOUNT_PATH}" \
        --dns-cloudflare-propagation-seconds 30 \
        -d "${TARGET_DOMAIN}" \
        -d "*.${TARGET_DOMAIN}" \
        --email "${CERTBOT_EMAIL}" \
        --agree-tos \
        --no-eff-email \
        --non-interactive
else
    docker compose run --rm --entrypoint certbot certbot certonly \
        --dns-cloudflare \
        --dns-cloudflare-credentials "${CF_MOUNT_PATH}" \
        --dns-cloudflare-propagation-seconds 30 \
        -d "${TARGET_DOMAIN}" \
        -d "*.${TARGET_DOMAIN}" \
        --email "${CERTBOT_EMAIL}" \
        --agree-tos \
        --no-eff-email \
        --non-interactive
fi

echo -e "\n${GREEN}========================================================================${NC}"
echo -e "${GREEN}[✓] ¡Certificados TLS Wildcard generados para '${TARGET_DOMAIN}'!${NC}"
echo -e "${GREEN}    Ruta Certificado: /etc/letsencrypt/live/${TARGET_DOMAIN}/fullchain.pem${NC}"
echo -e "${GREEN}    Ruta Clave:       /etc/letsencrypt/live/${TARGET_DOMAIN}/privkey.pem${NC}"
echo -e "${GREEN}========================================================================${NC}"
echo -e "${CYAN}[Próximo paso] En tu archivo de Nginx ('nginx/conf.d/<proyecto>.conf'):${NC}"
echo -e "  server_name ${TARGET_DOMAIN} *.${TARGET_DOMAIN};"
echo -e "  ssl_certificate     /etc/letsencrypt/live/${TARGET_DOMAIN}/fullchain.pem;"
echo -e "  ssl_certificate_key /etc/letsencrypt/live/${TARGET_DOMAIN}/privkey.pem;"
echo -e "  Luego ejecuta: ./scripts/reload.sh\n"
