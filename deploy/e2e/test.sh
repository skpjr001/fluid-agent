#!/usr/bin/env bash
# Runs fluid-agent behind nginx, Traefik and Caddy and checks every layout:
# subdomain root, path with and without prefix stripping, console + API + SSE
# under a path, and the forward proxy through TCP passthrough.
set -uo pipefail
cd "$(dirname "$0")"
COMPOSE=${COMPOSE:-docker compose}
TOKEN=e2e-viewer-token-0123456789
CLIENT=e2e-client-token-0123456789
fail=0
check() { # name, expected, actual
  if [[ "$3" == *"$2"* ]]; then printf '  ok    %s\n' "$1"; else printf '  FAIL  %s\n        want %q, got %q\n' "$1" "$2" "${3:0:200}"; fail=1; fi
}

# Test certificates: a private CA + leaf for tls.test, and Pebble's CA for ACME.
rm -rf .certs && mkdir -p .certs
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 -subj "/CN=e2e CA" \
  -keyout .certs/ca.key -out .certs/ca.pem 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=tls.test" \
  -keyout .certs/tls.key -out .certs/tls.csr 2>/dev/null
printf 'subjectAltName=DNS:tls.test,DNS:localhost,IP:127.0.0.1\n' > .certs/ext
openssl x509 -req -in .certs/tls.csr -CA .certs/ca.pem -CAkey .certs/ca.key -CAcreateserial -days 1 \
  -extfile .certs/ext -out .certs/tls.crt 2>/dev/null
chmod 644 .certs/*                                   # readable by the container's non-root user

$COMPOSE up -d --quiet-pull pebble >/dev/null 2>&1
$COMPOSE cp pebble:/test/certs/pebble.minica.pem .certs/pebble.minica.pem >/dev/null
chmod 644 .certs/pebble.minica.pem
$COMPOSE up -d --build --quiet-pull >/dev/null 2>&1 || { $COMPOSE up -d --build; exit 1; }
trap '[[ $fail != 0 ]] && $COMPOSE logs --tail 40 agent agent-tls agent-acme-alpn agent-acme-http pebble; $COMPOSE down -v >/dev/null 2>&1; rm -rf .certs' EXIT
for i in $(seq 1 60); do curl -fs -H 'Host: hooks.test' http://127.0.0.1:28080/readyz >/dev/null && break; sleep 1; done

for proxy in nginx:28080:28088 traefik:38080:38088 caddy:48080:; do
  IFS=: read -r name http fwd <<<"$proxy"
  echo "== $name"
  check "webhook on subdomain root" 'Hostname:' "$(curl -s -X POST -H 'Host: hooks.test' "http://127.0.0.1:$http/hooks/w" -d x)"
  check "webhook under /hooks-gw" 'Hostname:' "$(curl -s -X POST -H 'Host: example.test' "http://127.0.0.1:$http/hooks-gw/hooks/w" -d x)"
  check "ingest under /hooks-gw" '{"status":"success"}' "$(curl -s -X POST -H 'Host: example.test' "http://127.0.0.1:$http/hooks-gw/v1/status" -d x)"
  check "console index carries base" 'src="/console/app.js"' "$(curl -s -H 'Host: example.test' "http://127.0.0.1:$http/console/")"
  check "console deep link" 'src="/console/app.js"' "$(curl -s -H 'Host: example.test' "http://127.0.0.1:$http/console/events")"
  check "console asset" 'fluid-agent console' "$(curl -s -H 'Host: example.test' "http://127.0.0.1:$http/console/app.js" | head -c 300)"
  check "API under /console" '"role": "viewer"' "$(curl -s -H 'Host: example.test' -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$http/console/api/whoami")"
  check "live stream under /console" ': connected' "$(curl -s -N --max-time 2 -H 'Host: example.test' -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:$http/console/api/stream?topics=logs")"
  if [[ -n "$fwd" ]]; then
    check "forward proxy via TCP passthrough" 'Hostname:' "$(curl -s --max-time 5 -x "http://e2e:$CLIENT@127.0.0.1:$fwd" http://whoami/)"
    check "proxy refuses bad token" '407' "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -x "http://e2e:wrong-token-000000@127.0.0.1:$fwd" http://whoami/)"
  fi
done

echo "== sender address through each proxy (caller 172.32.0.50 on a separate network)"
for host in nginx traefik caddy; do
  $COMPOSE exec -T client curl -s -o /dev/null -X POST -H 'Host: hooks.test' "http://$host/hooks/w" -d x
  sleep 0.5
  got=$(curl -s -H 'Host: example.test' -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:28080/console/api/logs?kind=webhook&limit=1' | grep -o '"source_ip": "[^"]*"' | head -1)
  check "$host passes the real sender" '"source_ip": "172.32.0.50"' "$got"
done

echo "== built-in TLS, no reverse proxy (certificate files)"
R=(--resolve tls.test:58080:127.0.0.1 --resolve tls.test:58081:127.0.0.1 --resolve tls.test:59000:127.0.0.1 --cacert .certs/ca.pem)
for i in $(seq 1 30); do curl -fs "${R[@]}" https://tls.test:58081/livez >/dev/null && break; sleep 1; done
check "webhook over HTTPS" 'Hostname:' "$(curl -s "${R[@]}" -X POST https://tls.test:58081/hooks/w -d x)"
check "plain HTTP to TLS listener gets 400, nothing served" 'HTTPS server' "$(curl -s --max-time 3 http://127.0.0.1:58081/livez)"
check "console over HTTPS + HTTP/2" '2 200' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_version} %{http_code}' -H "Authorization: Bearer $TOKEN" https://tls.test:59000/console/api/whoami)"
check "console enforces TLS 1.3" '000' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_code}' --tls-max 1.2 https://tls.test:59000/console/)"
check "HTTPS proxy (absolute URL)" 'Hostname:' "$(curl -s "${R[@]}" --max-time 5 --proxy "https://e2e:$CLIENT@tls.test:58080" --proxy-cacert .certs/ca.pem http://whoami/)"
check "HTTPS proxy (CONNECT tunnel)" 'Hostname:' "$(curl -s "${R[@]}" --max-time 5 -p --proxy "https://e2e:$CLIENT@tls.test:58080" --proxy-cacert .certs/ca.pem http://whoami/)"
check "HTTPS proxy refuses bad token" '407' "$(curl -s "${R[@]}" -o /dev/null -w '%{http_code}' --max-time 5 --proxy "https://e2e:bad-token-0000000000@tls.test:58080" --proxy-cacert .certs/ca.pem http://whoami/)"

echo "== ACME certificates from Pebble"
T="$COMPOSE exec -T tester curl"
for host in acme-alpn.test:443 acme-http.test:8443; do
  name=${host%%:*}
  out=""
  for i in $(seq 1 30); do out=$($T -sk -v --max-time 20 "https://$host/livez" 2>&1) && [[ "$out" == *"ok"* ]] && break; sleep 2; done
  check "$name: certificate issued and served" 'Pebble Intermediate' "$out"
done
check "acme-http.test: HTTP redirects to HTTPS" 'Location: https://acme-http.test' "$($T -s -D - -o /dev/null http://acme-http.test/hooks/w)"

echo "== deployment notes logged for each setup"
logs() { $COMPOSE logs --no-log-prefix "$1" 2>/dev/null | grep '"kind":"deploy"'; }
check "agent: container detected" 'running in a' "$(logs agent | grep -o 'running in a [a-z]* container')"
check "agent: image VOLUME counts as storage (no false warning)" 'no-volume-warning' "$(logs agent | grep -q 'not on a mounted volume' && echo warned || echo no-volume-warning)"
check "agent: read-only config mount warned" '"level":"WARN","msg":"dynamic config dir /app/dynamic is read-only' "$(logs agent)"
check "agent: reverse proxy trust explained" 'expecting TLS to be terminated by a reverse proxy at 172.31.0.0/24' "$(logs agent)"
check "agent: console base path" 'console at /console/' "$(logs agent)"
check "agent: open webhook warned" '"level":"WARN","msg":"webhook w accepts requests from anyone' "$(logs agent)"
check "agent-tls: HTTPS without proxy" 'webhooks are served over HTTPS directly' "$(logs agent-tls)"
check "agent-tls: certificate details" 'certificate /certs/tls.crt for tls.test, localhost, 127.0.0.1' "$(logs agent-tls)"
check "agent-tls: HTTPS proxy note" 'HTTPS proxy: clients use https://' "$(logs agent-tls)"
check "agent-acme-alpn: TLS-ALPN-01" 'TLS-ALPN-01 challenges on port 443' "$(logs agent-acme-alpn)"
check "agent-acme-http: HTTP-01" 'HTTP-01 challenges on :80' "$(logs agent-acme-http)"

[[ $fail == 0 ]] && echo "ALL E2E CHECKS PASSED" || echo "E2E FAILURES"
exit $fail
