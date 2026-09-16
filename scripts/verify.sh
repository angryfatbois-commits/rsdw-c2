#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
mkdir -p .tmp

go test ./...
go vet ./...
node --check web/app.js
node --test web/app.test.cjs
node <<'NODE'
const fs = require('fs');
const source = fs.readFileSync('web/app.js', 'utf8') + fs.readFileSync('web/index.html', 'utf8');
const rendered = [...source.matchAll(/data-action="([^"]+)"/g)].map((match) => match[1]);
const handled = [...fs.readFileSync('web/app.js', 'utf8').matchAll(/case '([^']+)'/g)].map((match) => match[1]);
const missing = [...new Set(rendered)].filter((action) => !handled.includes(action));
if (missing.length) throw new Error(`Unhandled UI actions: ${missing.join(', ')}`);
NODE
go build -o .tmp/rsdw-c2 .
helm lint charts/rsdw-c2 --set auth.adminTokenSecret.name=rsdw-c2-admin
helm template rsdw-c2 charts/rsdw-c2 --namespace rsdw-system --set auth.adminTokenSecret.name=rsdw-c2-admin >/tmp/rsdw-c2-manifest.yaml
if helm template rsdw-c2 charts/rsdw-c2 >/dev/null 2>&1; then
  printf '%s\n' 'chart rendered without the required admin Secret' >&2
  exit 1
fi
grep -q 'kind: Deployment' /tmp/rsdw-c2-manifest.yaml
grep -q 'kind: ClusterRole' /tmp/rsdw-c2-manifest.yaml
grep -q 'containerPort: 8080' /tmp/rsdw-c2-manifest.yaml

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
