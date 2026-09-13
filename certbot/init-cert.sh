#!/bin/bash
# ==============================================================================
# init-cert.sh: Emisión Inicial de Certificados Wildcard DNS-01 con Cloudflare
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

ENV_FILE="${REPO_ROOT}/.env"
CF_INI="${SCRIPT_DIR}/cloudflare.ini"
CF_EXAMPLE="${SCRIPT_DIR}/cloudflare.ini.example"

# Colores para la terminal
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

echo -e "${BLUE}=== [Certbot DNS-01 Cloudflare] Inicialización de Certificados Wildcard ===${NC}"

# 1. Cargar variables desde .env
if [ -f "${ENV_FILE}" ]; then
    # shellcheck disable=SC2046
    export $(grep -v '^#' "${ENV_FILE}" | xargs)
else
    echo -e "${RED}[!] ERROR: No se encontró el archivo ${ENV_FILE}.${NC}"
    echo "    Copia .env.example a .env y define tus variables."
    exit 1
fi

if [ -z "${BASE_DOMAIN:-}" ] || [ "${BASE_DOMAIN}" = "tudominio.com" ]; then
    echo -e "${RED}[!] ERROR: BASE_DOMAIN no está configurado correctamente en .env.${NC}"
    exit 1
fi

if [ -z "${CERTBOT_EMAIL:-}" ] || [ "${CERTBOT_EMAIL}" = "admin@tudominio.com" ]; then
    echo -e "${RED}[!] ERROR: CERTBOT_EMAIL no está configurado correctamente en .env.${NC}"
    exit 1
fi

# 2. Validar archivo cloudflare.ini
if [ ! -f "${CF_INI}" ]; then
    echo -e "${RED}[!] ERROR: No se encuentra ${CF_INI}.${NC}"
    echo -e "${YELLOW}[i] Copia ${CF_EXAMPLE} a ${CF_INI} y coloca tu API Token de Cloudflare.${NC}"
    exit 1
fi

if grep -q "0123456789abcdef0123456789abcdef01234567" "${CF_INI}"; then
    echo -e "${RED}[!] ERROR: Debes reemplazar el token de ejemplo en ${CF_INI} con tu token real.${NC}"
    exit 1
fi

# 3. Asegurar permisos estrictos requeridos por certbot-dns-cloudflare (chmod 600)
chmod 600 "${CF_INI}"
echo -e "${GREEN}[✓] Permisos en cloudflare.ini ajustados a 600.${NC}"

# 4. Verificar si ya existe certificado emitido
if [ -d "${SCRIPT_DIR}/conf/live/${BASE_DOMAIN}" ]; then
    echo -e "${YELLOW}[!] AVISO: Ya existe un certificado para ${BASE_DOMAIN} en certbot/conf/live/${BASE_DOMAIN}.${NC}"
    read -r -p "¿Deseas forzar la renovación/re-emisión? (s/N): " response
    if [[ ! "$response" =~ ^([sS][iI]|[sS])$ ]]; then
        echo "Operación cancelada."
        exit 0
    fi
fi

# 5. Ejecutar Certbot vía Docker Compose
echo -e "${BLUE}[+] Solicitando certificado para '${BASE_DOMAIN}' y '*.${BASE_DOMAIN}' mediante DNS-01...${NC}"

cd "${REPO_ROOT}"
docker compose run --rm --entrypoint certbot certbot certonly \
    --dns-cloudflare \
    --dns-cloudflare-credentials /etc/letsencrypt/cloudflare.ini \
    --dns-cloudflare-propagation-seconds 30 \
    -d "${BASE_DOMAIN}" \
    -d "*.${BASE_DOMAIN}" \
    --email "${CERTBOT_EMAIL}" \
    --agree-tos \
    --no-eff-email \
    --non-interactive

echo -e "${GREEN}========================================================================${NC}"
echo -e "${GREEN}[✓] ¡Certificados TLS Wildcard generados exitosamente!${NC}"
echo -e "${GREEN}    Ruta: certbot/conf/live/${BASE_DOMAIN}/fullchain.pem${NC}"
echo -e "${GREEN}    Clave: certbot/conf/live/${BASE_DOMAIN}/privkey.pem${NC}"
echo -e "${GREEN}========================================================================${NC}"
echo -e "${BLUE}[i] Ya puedes iniciar o recargar Nginx con: docker compose up -d nginx${NC}"
