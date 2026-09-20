#!/bin/sh
# Prueba de punta a punta: API + nodo + agente + backends, todo en Docker.
# Requiere las imágenes :dev (ver Dockerfile). Uso: test/e2e/run.sh
set -eu
cd "$(dirname "$0")"
dc() { docker compose "$@"; }
fail() { echo "FALLO: $*"; dc --profile relay --profile standby logs --tail 40; exit 1; }
# trap deshabilitado

dc down -v >/dev/null 2>&1 || true

# CA de la API de Pebble, para que el agente confíe en ese servidor ACME.
cid=$(docker create ghcr.io/letsencrypt/pebble:latest)
docker cp "$cid:/test/certs/pebble.minica.pem" ./minica.pem >/dev/null
docker rm "$cid" >/dev/null

dc up -d --wait postgres api mockcf >/dev/null

out=$(dc exec -T api wgrelay-api tunnel create --email e2e@test)
export AGENT_TOKEN=$(echo "$out" | grep -o 'wgr_[a-z0-9_]*')
DOMAIN=$(echo "$out" | awk '/dominio:/{print $2}')
export NODE_TOKEN=$(dc exec -T api wgrelay-api node create --name node1 --endpoint node:51820 --public-ip 203.0.113.10 | grep -o 'wgn_[a-z0-9_]*')
export TERMINATE_HOST="web.$DOMAIN"
export TERMINATE_HOST_TCP="mqttterm.$DOMAIN"
WILDCARD_HOST="cualquiercosa.wild.$DOMAIN"
echo "tunnel: $DOMAIN"

dc --profile relay up -d >/dev/null

# Raíz efímera con la que Pebble firma los certificados que emite.
for i in $(seq 1 30); do
  curl -sk --max-time 3 https://127.0.0.1:15000/roots/0 -o pebble-root.pem && [ -s pebble-root.pem ] && break
  sleep 1
done
[ -s pebble-root.pem ] || fail "no se pudo obtener la raíz de Pebble"

probe() {
  curl -sk --max-time 3 --resolve "$1:18443:127.0.0.1" "https://$1:18443/"
}
wait_sni() {
  for i in $(seq 1 30); do probe "$1" | grep -q 's_server' && return 0; sleep 1; done
  return 1
}
# Con validación real del certificado emitido por la CA de prueba.
probe_web() {
  curl -s --max-time 5 --cacert pebble-root.pem --resolve "$TERMINATE_HOST:18443:127.0.0.1" "https://$TERMINATE_HOST:18443/"
}
wait_web() {
  for i in $(seq 1 60); do [ -n "$(probe_web || true)" ] && return 0; sleep 2; done
  return 1
}

echo "1. passthrough por SNI hasta el backend"
wait_sni "mqtt.$DOMAIN" || fail "no llegó al backend"
echo "   ok"

echo "2. una ruta comodín cubre subdominios no enumerados"
wait_sni "loquesea.dev.$DOMAIN" || fail "el comodín no ruteó"
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

echo "5. modo terminate: el agente emite su certificado por ACME y proxea HTTP"
wait_web || fail "no llegó al backend en modo terminate"
probe_web | grep -q 'X-Forwarded-For' || fail "no se propagó la IP real del visitante"
echo "   ok"

echo "6. el certificado sobrevive al reinicio del agente (almacén cifrado)"
# El agente tiene dos hosts en terminate (web y mqttterm): contamos por host,
# no en total, para no confundir "dos hosts" con "reemisión".
issued_web=$(dc logs agent | grep -c "certificate obtained successfully.*$TERMINATE_HOST\b" || true)
[ "$issued_web" = "1" ] || fail "se esperaba una única emisión para $TERMINATE_HOST, hubo $issued_web"
dc restart agent >/dev/null
wait_web || fail "el agente no volvió a servir después del reinicio"
issued_web=$(dc logs agent | grep -c "certificate obtained successfully.*$TERMINATE_HOST\b" || true)
[ "$issued_web" = "1" ] || fail "reemitió el certificado de $TERMINATE_HOST en vez de cargarlo del almacén ($issued_web)"
echo "   ok"

echo "7. terminate con comodín: certificado wildcard vía DNS-01 (mockcf, cloudflare+DNS de juguete)"
probe_wildcard() {
  curl -s --max-time 5 --cacert pebble-root.pem --resolve "$WILDCARD_HOST:18443:127.0.0.1" "https://$WILDCARD_HOST:18443/"
}
ok=""
for i in $(seq 1 60); do ok=$(probe_wildcard || true); [ -n "$ok" ] && break; sleep 2; done
[ -n "$ok" ] || fail "el comodín en terminate no sirvió (DNS-01 no funcionó)"
dc logs api | grep -q 'DNS-01: TXT creado' || fail "la API no registra haber creado el TXT"
dc logs mockcf | grep -qi 'CREATE TXT' || fail "mockcf no recibió el TXT"
echo "   ok"

echo "8. export_cert: el agente exporta fullchain.pem/privkey.pem para el backend"
for i in $(seq 1 60); do
  dc cp agent:/certs/exported/fullchain.pem exported-fullchain.pem >/dev/null 2>&1 && break
  sleep 2
done
[ -s exported-fullchain.pem ] || fail "export_cert no escribió fullchain.pem"
dc cp agent:/certs/exported/privkey.pem exported-privkey.pem >/dev/null 2>&1 || fail "export_cert no escribió privkey.pem"
openssl x509 -in exported-fullchain.pem -noout -text | grep -q "exported.$DOMAIN" || fail "el certificado exportado no cubre exported.$DOMAIN"
openssl x509 -in exported-fullchain.pem -noout -checkend 0 >/dev/null || fail "el certificado exportado no es válido"
rm -f exported-fullchain.pem exported-privkey.pem
echo "   ok"

echo "9. una segunda instancia con el mismo token queda en espera"
dc --profile standby up -d agent2 >/dev/null
sleep 3
dc logs agent2 | grep -q 'otra instancia' || fail "agent2 no quedó en espera"
wait_sni "mqtt.$DOMAIN" || fail "agent2 le robó el túnel al primero"
echo "   ok"

echo "10. al detener la primera, la segunda toma el túnel"
dc stop agent >/dev/null
wait_sni "mqtt.$DOMAIN" || fail "agent2 no tomó el túnel"
dc logs agent2 | grep -q 'Dominio:' || fail "agent2 no registró"
echo "   ok"

echo "11. la segunda instancia reusa el certificado del almacén, no reemite"
wait_web || fail "agent2 no sirve el modo terminate"
issued=$(dc logs agent2 | grep -c 'certificate obtained successfully' || true)
[ "$issued" = "0" ] || fail "agent2 reemitió el certificado ($issued)"
echo "   ok"

echo "12. terminate tcp://: el agente termina TLS y entrega MQTT plano al backend"
mqtt_roundtrip() {
  docker run --rm --network wgrelay-e2e_default -v "$PWD/pebble-root.pem:/ca.pem:ro" eclipse-mosquitto:2 sh -c "
    mosquitto_sub -h $TERMINATE_HOST_TCP -p 8443 --cafile /ca.pem -t e2e/tcp -C 1 -W 10 &
    sleep 2
    mosquitto_pub -h $TERMINATE_HOST_TCP -p 8443 --cafile /ca.pem -t e2e/tcp -m ok
    wait" 2>/dev/null
}
out=""
for i in $(seq 1 40); do out=$(mqtt_roundtrip); [ "$out" = "ok" ] && break; sleep 2; done
[ "$out" = "ok" ] || fail "MQTT sobre terminate tcp:// no funcionó (out=$out)"
echo "   ok"

echo "13. DNS autoritativo propio del nodo (clients.e2e.test, sin Cloudflare)"
dnsq() {
  docker run --rm --network wgrelay-e2e_default \
    -v "$PWD/dnsquery:/src:ro" -v gomod_cache:/go/pkg/mod -w /src \
    golang:1-alpine go run . -server node:5300 "$@" 2>/dev/null
}
curl_e2e() { docker run --rm --network wgrelay-e2e_default curlimages/curl -s "$@"; }

dnsq -type SOA -name clients.e2e.test | grep -q 'hostmaster' || fail "SOA del ápice no respondió"
dnsq -type NS -name clients.e2e.test | grep -q 'ns1.e2e.test' || fail "NS del ápice no respondió"
dnsq -type A -name "$DOMAIN" | grep -q '203.0.113.10' || fail "catch-all no devolvió la IP del nodo activo"
dnsq -type A -name "cualquiercosa.$DOMAIN" | grep -q '203.0.113.10' || fail "catch-all no cubrió un subdominio no enumerado"
dnsq -type A -name "otra-cosa.invalid" -expect-refused >/dev/null || fail "respondió autoritativamente fuera de su zona"

# Ciclo completo de un TXT real: crear vía la API, ver que se sirve, borrar,
# ver que desaparece. Ejercita toda la cadena API -> DB -> long-poll ->
# node.apply() -> dnsserver.Zone -> respuesta UDP/TCP real. Va antes de
# rotar el token (paso 14): usa $AGENT_TOKEN, que ese paso invalida.
challenge_id=$(curl_e2e -X POST -H "Authorization: Bearer $AGENT_TOKEN" \
  -d '{"fqdn":"_acme-challenge.'"$DOMAIN"'","value":"selfhosted-e2e-value"}' \
  "http://api:8080/v1/agent/acme-dns" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$challenge_id" ] || fail "no se pudo crear el desafío TXT de prueba"
ok=""
for i in $(seq 1 10); do
  ok=$(dnsq -type TXT -name "_acme-challenge.$DOMAIN" | grep -c 'selfhosted-e2e-value' || true)
  [ "$ok" = "1" ] && break
  sleep 1
done
[ "$ok" = "1" ] || fail "el TXT autohospedado no se sirvió"
curl_e2e -o /dev/null -X DELETE -H "Authorization: Bearer $AGENT_TOKEN" \
  "http://api:8080/v1/agent/acme-dns/$challenge_id"
gone=""
for i in $(seq 1 10); do
  gone=$(dnsq -type TXT -name "_acme-challenge.$DOMAIN" | grep -c 'selfhosted-e2e-value' || true)
  [ "$gone" = "0" ] && break
  sleep 1
done
[ "$gone" = "0" ] || fail "el TXT autohospedado no se borró"
echo "   ok"

echo "14. rotar el token desconecta al agente"
dc exec -T api wgrelay-api tunnel rotate-token --id 1 >/dev/null
for i in $(seq 1 25); do dc ps agent2 --status exited | grep -q agent2 && break; sleep 1; done
dc logs agent2 | grep -q 'rechazó el token' || fail "agent2 no detectó el token rotado"
echo "   ok"

echo "TODO OK"
