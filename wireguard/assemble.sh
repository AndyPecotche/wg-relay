#!/bin/bash
# ==============================================================================
# assemble.sh: Ensamblador y Sincronizador en Caliente de WireGuard
# ==============================================================================
set -euo pipefail

TEMPLATES_DIR="${TEMPLATES_DIR:-/templates}"
PEERS_DIR="${PEERS_DIR:-/peers.d}"
CONFIG_DIR="${CONFIG_DIR:-/config}"
INTERFACE_TEMPLATE="${TEMPLATES_DIR}/interface.conf"
OUTPUT_CONF="${CONFIG_DIR}/wg0.conf"
TEMP_CONF="${CONFIG_DIR}/wg0.conf.tmp"
SERVER_KEY="${CONFIG_DIR}/server.key"
SERVER_PUB="${CONFIG_DIR}/server.pub"

echo "=== [WireGuard Modular Assembler] Iniciando compilación ==="

# 1. Asegurar la existencia de las llaves del servidor Relay
if [ ! -f "${SERVER_KEY}" ]; then
    echo "[+] Generando nuevo par de claves para el servidor Relay..."
    wg genkey > "${SERVER_KEY}"
    chmod 600 "${SERVER_KEY}"
    wg pubkey < "${SERVER_KEY}" > "${SERVER_PUB}"
    echo "[✓] Clave privada y pública creadas en ${CONFIG_DIR}."
fi

SERVER_PRIVATE_KEY=$(cat "${SERVER_KEY}" | tr -d '\r\n ')
SERVER_PUBLIC_KEY=$(cat "${SERVER_PUB}" | tr -d '\r\n ')

echo "[i] Clave Pública del Relay VPS: ${SERVER_PUBLIC_KEY}"

# 2. Validar que exista la plantilla base
if [ ! -f "${INTERFACE_TEMPLATE}" ]; then
    echo "[!] ERROR CRÍTICO: No se encuentra la plantilla ${INTERFACE_TEMPLATE}" >&2
    exit 1
fi

# 3. Compilar interfaz base reemplazando el placeholder de la clave privada
echo "[+] Procesando plantilla base: ${INTERFACE_TEMPLATE}"
sed "s|__SERVER_PRIVATE_KEY__|${SERVER_PRIVATE_KEY}|g" "${INTERFACE_TEMPLATE}" > "${TEMP_CONF}"

# 4. Concatenar cada peer modular (.conf) ignorando .example
PEER_COUNT=0
if [ -d "${PEERS_DIR}" ]; then
    for peer_file in "${PEERS_DIR}"/*.conf; do
        # Verificar si hay archivos coincidentes (evitar el glob literal si está vacío)
        [ -e "${peer_file}" ] || continue
        
        # Ignorar archivos que terminen en .example o temporales
        case "${peer_file}" in
            *.example|*.bak|*.tmp) continue ;;
        esac

        peer_name=$(basename "${peer_file}")
        echo "  -> Incorporando peer: ${peer_name}"
        echo "" >> "${TEMP_CONF}"
        echo "# ----------------------------------------------------------------------" >> "${TEMP_CONF}"
        echo "# Peer: ${peer_name}" >> "${TEMP_CONF}"
        echo "# ----------------------------------------------------------------------" >> "${TEMP_CONF}"
        cat "${peer_file}" >> "${TEMP_CONF}"
        PEER_COUNT=$((PEER_COUNT + 1))
    done
fi

echo "[✓] Total de peers activos ensamblados: ${PEER_COUNT}"

# 5. Guardar archivo final de forma atómica
mv "${TEMP_CONF}" "${OUTPUT_CONF}"
chmod 600 "${OUTPUT_CONF}"
echo "[✓] Archivo ${OUTPUT_CONF} generado exitosamente."

# 6. Sincronizar en caliente si la interfaz wg0 ya está activa
if ip link show wg0 >/dev/null 2>&1; then
    echo "[+] Interfaz wg0 detectada en ejecución. Aplicando wg syncconf en caliente..."
    # wg-quick strip limpia las directivas de alto nivel (Address, PostUp, etc.) para wg syncconf
    wg syncconf wg0 <(wg-quick strip "${OUTPUT_CONF}")
    echo "[✓] Sincronización en caliente completada con éxito. Cero downtime."
else
    echo "[i] Interfaz wg0 aún no levantada. El archivo se utilizará en el arranque."
fi

echo "=== [WireGuard Modular Assembler] Finalizado ==="
