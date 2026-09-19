#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
mkdir -p .tmp

go test ./...
go vet ./...
python3 verification/check-tick-evidence.py verification/tick-live-observations.json
node --check web/app.js
node --test web/app.test.cjs
node --test verification/save-import.test.cjs
bash scripts/verify-auth-chart.sh
bash scripts/verify-memory-pressure-chart.sh
node <<'NODE'
const fs = require('fs');
const source = fs.readFileSync('web/app.js', 'utf8') + fs.readFileSync('web/index.html', 'utf8');
const rendered = [...source.matchAll(/data-action="([^"]+)"/g)].map((match) => match[1]);
const handled = [...fs.readFileSync('web/app.js', 'utf8').matchAll(/case '([^']+)'/g)].map((match) => match[1]);
const missing = [...new Set(rendered)].filter((action) => !handled.includes(action));
if (missing.length) throw new Error(`Unhandled UI actions: ${missing.join(', ')}`);
NODE
go build -o .tmp/rsdw-c2 .
node tests/users-browser.cjs
node tests/deletion-browser.cjs
node tests/dashboard-browser.cjs
node tests/events-browser.cjs
node tests/integrations-browser.cjs
node tests/oidc-browser.cjs
node tests/settings-browser.cjs
node tests/player-roster-browser.cjs
node tests/stop-browser.cjs
helm lint charts/rsdw-c2 --set auth.adminTokenSecret.name=rsdw-c2-admin
helm template rsdw-c2 charts/rsdw-c2 --namespace rsdw-system --set auth.adminTokenSecret.name=rsdw-c2-admin >/tmp/rsdw-c2-manifest.yaml
helm template rsdw-c2 charts/rsdw-c2 --namespace rsdw-system \
  --set auth.adminTokenSecret.name=rsdw-c2-admin \
  --set ingress.enabled=true \
  --set ingress.ingressClassName=nginx \
  --set 'ingress.hosts[0]=c2.example.com' \
  --set 'ingress.hosts[1]=admin.example.com' \
  --set 'ingress.paths[0].path=/' \
  --set 'ingress.paths[0].pathType=Prefix' \
  --set 'ingress.paths[1].path=/api' \
  --set 'ingress.paths[1].pathType=Prefix' \
  --set-string 'ingress.annotations.example\.com/auth=enabled' \
  --set 'ingress.tls[0].secretName=rsdw-c2-tls' \
  --set 'ingress.tls[0].hosts[0]=c2.example.com' \
  --show-only templates/ingress.yaml >/tmp/rsdw-c2-ingress.yaml
helm template rsdw-c2 charts/rsdw-c2 --namespace rsdw-system \
  --set auth.adminTokenSecret.name=rsdw-c2-admin \
  --set ingress.enabled=true \
  --set service.port=9090 >/tmp/rsdw-c2-port-manifest.yaml
if helm template rsdw-c2 charts/rsdw-c2 >/dev/null 2>&1; then
  printf '%s\n' 'chart rendered without the required admin Secret' >&2
  exit 1
fi
grep -q 'kind: Deployment' /tmp/rsdw-c2-manifest.yaml
grep -q 'kind: ClusterRole' /tmp/rsdw-c2-manifest.yaml
grep -q 'type: ClusterIP' /tmp/rsdw-c2-manifest.yaml
grep -q 'containerPort: 8080' /tmp/rsdw-c2-manifest.yaml
if grep -q 'kind: Ingress' /tmp/rsdw-c2-manifest.yaml; then
  printf '%s\n' 'chart rendered an Ingress while ingress.enabled=false' >&2
  exit 1
fi
grep -q 'apiVersion: networking.k8s.io/v1' /tmp/rsdw-c2-ingress.yaml
grep -q 'kind: Ingress' /tmp/rsdw-c2-ingress.yaml
grep -q 'ingressClassName: "nginx"' /tmp/rsdw-c2-ingress.yaml
grep -q 'example.com/auth: enabled' /tmp/rsdw-c2-ingress.yaml
grep -q 'host: "c2.example.com"' /tmp/rsdw-c2-ingress.yaml
grep -q 'host: "admin.example.com"' /tmp/rsdw-c2-ingress.yaml
grep -q 'path: "/"' /tmp/rsdw-c2-ingress.yaml
grep -q 'path: "/api"' /tmp/rsdw-c2-ingress.yaml
[[ $(grep -c 'path: ' /tmp/rsdw-c2-ingress.yaml) -eq 4 ]]
grep -q 'pathType: Prefix' /tmp/rsdw-c2-ingress.yaml
grep -q 'name: rsdw-c2-rsdw-c2' /tmp/rsdw-c2-ingress.yaml
grep -q 'name: http' /tmp/rsdw-c2-ingress.yaml
grep -q 'secretName: rsdw-c2-tls' /tmp/rsdw-c2-ingress.yaml
grep -q 'port: 9090' /tmp/rsdw-c2-port-manifest.yaml
grep -q 'name: http' /tmp/rsdw-c2-port-manifest.yaml
if grep -q 'number: 8080' /tmp/rsdw-c2-port-manifest.yaml; then
  printf '%s\n' 'Ingress backend hard-coded port 8080' >&2
  exit 1
fi

test_dir=$(mktemp -d -t rsdw-c2-verify.XXXXXX)
RSDW_DEMO_DATA=true RSDW_STATE_FILE="$test_dir/state.json" RSDW_LISTEN_ADDR="127.0.0.1:0" .tmp/rsdw-c2 >"$test_dir/server.log" 2>&1 &
pid=$!
cleanup() { kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; }
trap cleanup EXIT
for _ in $(seq 1 30); do
  kill -0 "$pid"
  port=$(sed -n 's/.*listening on 127.0.0.1:\([0-9]*\).*/\1/p' "$test_dir/server.log")
  [[ -n "$port" ]] && break
  sleep 0.2
done
[[ -n "$port" ]]
curl -fsS "http://127.0.0.1:$port/api/bootstrap" | grep -q 'ScuffedTards'
curl -fsS "http://127.0.0.1:$port/" | grep -q 'DRAGONWILDS'
curl -fsS "http://127.0.0.1:$port/api/servers/scuffedtards/logs?tail=5" | grep -q 'health check'
curl -fsS "http://127.0.0.1:$port/api/servers/scuffedtards/telemetry?range=60s" | grep -q 'samples'
curl -fsS "http://127.0.0.1:$port/api/events?query=update" | grep -q 'Update available'
curl -fsS -X POST -H 'Content-Type: application/json' "http://127.0.0.1:$port/api/servers/scuffedtards/actions/check-update" | grep -q 'updateAvailable'

printf '%s\n' 'verify.sh passed'
