# Plantilla de Proyecto Local (Lado Cliente / CGNAT)

Esta carpeta contiene la arquitectura recomendada para correr en el servidor, mini-PC, Raspberry Pi o HomeLab de cualquier proyecto (`proyecto1`, `proyecto2`, etc.) detrás de CGNAT.

---

## Cómo Funciona

```
Internet (HTTPS)
       │
       ▼
   VPS Relay (Termina TLS con Wildcard *.tudominio.com)
       │
       ▼ (Túnel WireGuard UDP 51820)
       │
┌──────┴────────────────────────────────────────────────────────────────────────┐
│ Servidor Local de 'proyecto1' (Detrás de CGNAT)                              │
│                                                                               │
│  [wireguard-client] (IP VPN: 10.10.1.2)                                       │
│          ▲                                                                    │
│          │ (Comparte red con network_mode: service:wireguard-client)          │
│          ▼                                                                    │
│  [nginx-local] (Escucha en 10.10.1.2:80)                                      │
│          ├── proyecto1.tudominio.com      ──► [app-web] (172.30.0.10:80)      │
│          └── api.proyecto1.tudominio.com  ──► [app-api] (172.30.0.11:80)      │
└───────────────────────────────────────────────────────────────────────────────┘
```

---

## Instrucciones de Uso

1. **En el VPS Relay:**
   Registra el nuevo proyecto:
   ```bash
   ./scripts/add-peer.sh proyecto1 10.10.1.2
   ```
   Copia la plantilla Ingress en el VPS:
   ```bash
   cp nginx/conf.d/proyecto1.conf.example nginx/conf.d/proyecto1.conf
   ./scripts/reload.sh
   ```
   *¡Listo! Ya no tienes que tocar el VPS nunca más para este proyecto.*

2. **En tu máquina o servidor local (`proyecto1`):**
   Copia esta carpeta `examples/client-project` a tu servidor local.

3. **Configurar WireGuard:**
   Copia el archivo `wireguard/wg0.conf.example` a `wireguard/wg0.conf` y pega las claves generadas por `add-peer.sh`:
   ```bash
   cp wireguard/wg0.conf.example wireguard/wg0.conf
   nano wireguard/wg0.conf
   ```

4. **Levantar el stack local:**
   ```bash
   docker compose up -d
   ```

5. **Sumar nuevos microservicios en el futuro:**
   - Agrega tu nuevo contenedor en `docker-compose.yml` (ej. `grafana`).
   - Agrega un bloque `server` en `nginx/conf.d/default.conf` para `grafana.proyecto1.tudominio.com`.
   - Ejecuta `docker compose restart nginx-local`.
   - **No tocas el VPS.** La regla comodín `*.proyecto1.tudominio.com` del VPS ya te envía todo el tráfico automáticamente.

