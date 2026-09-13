#!/bin/bash
# ==============================================================================
# reload.sh: Recompilador y Recarga en Caliente de WireGuard y Nginx
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"

# Colores
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

TARGET="${1:-all}"

echo -e "${BLUE}=== [wg-relay] Recarga en Caliente de Servicios ===${NC}"

# Función para recargar WireGuard
reload_wireguard() {
    echo -e "${BLUE}[1/2] Sincronizando WireGuard...${NC}"
    if docker compose -f "${COMPOSE_FILE}" ps --status running --format json | grep -q "wireguard"; then
        docker compose -f "${COMPOSE_FILE}" exec -T wireguard /usr/local/bin/assemble.sh
        echo -e "${GREEN}[✓] WireGuard sincronizado exitosamente.${NC}"
    else
        echo -e "${YELLOW}[!] El contenedor 'wireguard' no está corriendo. Omitiendo syncconf en vivo.${NC}"
    fi
}

# Función para recargar Nginx
reload_nginx() {
    echo -e "${BLUE}[2/2] Validando y recargando Nginx...${NC}"
    if docker compose -f "${COMPOSE_FILE}" ps --status running --format json | grep -q "nginx"; then
        echo "  -> Comprobando sintaxis de configuración de Nginx (nginx -t)..."
        if docker compose -f "${COMPOSE_FILE}" exec -T nginx nginx -t; then
            echo "  -> Aplicando reload suave (cero downtime)..."
            docker compose -f "${COMPOSE_FILE}" exec -T nginx nginx -s reload
            echo -e "${GREEN}[✓] Nginx recargado exitosamente.${NC}"
        else
            echo -e "${RED}[!] ERROR: La sintaxis de Nginx es inválida. No se aplicó el reload.${NC}" >&2
            exit 1
        fi
    else
        echo -e "${YELLOW}[!] El contenedor 'nginx' no está corriendo. Omitiendo reload de Nginx.${NC}"
    fi
}

case "${TARGET}" in
    --wg-only)
        reload_wireguard
        ;;
    --nginx-only)
        reload_nginx
        ;;
    all|"")
        reload_wireguard
        reload_nginx
        ;;
    *)
        echo -e "${RED}[!] Opción desconocida: ${TARGET}${NC}"
        echo "Uso: $0 [--wg-only | --nginx-only | all]"
        exit 1
        ;;
esac

echo -e "${GREEN}=== [wg-relay] Recarga completada sin interrupción de servicio ===${NC}"

