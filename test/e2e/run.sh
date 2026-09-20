#!/bin/sh
# Prueba de punta a punta: API + nodo + agente + backend TLS en Docker.
# Requiere las imágenes :dev (ver Dockerfile). Uso: test/e2e/run.sh
set -eu
cd "$(dirname "$0")"
dc() { docker compose "$@"; }
fail() { echo "FALLO: $*"; dc --profile relay --profile standby logs --tail 40; exit 1; }
trap 'dc --profile relay --profile standby down -v >/dev/null 2>&1' EXIT

dc down -v >/dev/null 2>&1 || true
dc up -d --wait postgres api >/dev/null

out=$(dc exec -T api wgrelay-api tunnel create --email e2e@test)
export AGENT_TOKEN=$(echo "$out" | grep -o 'wgr_[a-z0-9_]*')
DOMAIN=$(echo "$out" | awk '/dominio:/{print $2}')
export NODE_TOKEN=$(dc exec -T api wgrelay-api node create --name node1 --endpoint node:51820 | grep -o 'wgn_[a-z0-9_]*')
echo "tunnel: $DOMAIN"

dc --profile relay up -d >/dev/null

probe() {
  curl -sk --max-time 3 --resolve "$1:18443:127.0.0.1" "https://$1:18443/"
}
wait_ok() {
  for i in $(seq 1 30); do probe "mqtt.$DOMAIN" | grep -q 's_server' && return 0; sleep 1; done
  return 1
}

echo "1. passthrough por SNI hasta el backend"
wait_ok || fail "no llegó al backend"
echo "   ok"

echo "2. una ruta comodin cubre subdominios no enumerados"
probe "loquesea.dev.$DOMAIN" | grep -q 's_server' || fail "el comodin no ruteo"
echo "   ok"

echo "3. hostname sin ruta se corta sin respuesta"
[ -z "$(probe "otro.$DOMAIN" || true)" ] || fail "respondió un host sin ruta"
[ -z "$(probe "nada.clients.e2e.test" || true)" ] || fail "respondió un tunnel inexistente"
echo "   ok"

echo "4. puerto 80 redirige hosts conocidos y corta el resto"
loc=$(curl -s -o /dev/null -w '%{redirect_url}' -H "Host: mqtt.$DOMAIN" http://127.0.0.1:18080/x)
[ "$loc" = "https://mqtt.$DOMAIN/x" ] || fail "redirect = $loc"
curl -s --max-time 3 -H "Host: desconocido.test" http://127.0.0.1:18080/ && fail "respondió host desconocido" || true
echo "   ok"

echo "5. una segunda instancia con el mismo token queda en espera"
dc --profile standby up -d agent2 >/dev/null
sleep 3
dc logs agent2 | grep -q 'otra instancia' || fail "agent2 no quedó en espera"
probe "mqtt.$DOMAIN" | grep -q s_server || fail "agent2 le robó el túnel al primero"
echo "   ok"

echo "6. al detener la primera, la segunda toma el túnel"
dc stop agent >/dev/null
wait_ok || fail "agent2 no tomó el túnel"
dc logs agent2 | grep -q 'Dominio:' || fail "agent2 no registró"
echo "   ok"

echo "7. rotar el token desconecta al agente"
dc exec -T api wgrelay-api tunnel rotate-token --id 1 >/dev/null
for i in $(seq 1 25); do dc ps agent2 --status exited | grep -q agent2 && break; sleep 1; done
dc logs agent2 | grep -q 'rechazó el token' || fail "agent2 no detectó el token rotado"
echo "   ok"

echo "TODO OK"
