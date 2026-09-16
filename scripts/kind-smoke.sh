#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
cluster_name="rsdw-c2-smoke-$$"
image_name=rsdw-c2:smoke

if ! docker info >/dev/null 2>&1; then
  printf '%s\n' 'INCONCLUSIVE: Docker access is required for kind-smoke.sh.' >&2
  exit 2
fi

test_dir=$(mktemp -d -t rsdw-c2-kind.XXXXXX)
export KUBECONFIG="$test_dir/kubeconfig"
cluster_created=false
forward_pid=
cleanup() {
  if [[ -n "$forward_pid" ]]; then kill "$forward_pid" 2>/dev/null || true; fi
  if "$cluster_created"; then kind delete cluster --name "$cluster_name"; fi
  printf 'Test logs: %s\n' "$test_dir"
}
trap cleanup EXIT

docker build -t "$image_name" .
kind create cluster --name "$cluster_name" --wait 120s
cluster_created=true
kind load docker-image "$image_name" --name "$cluster_name"
kubectl create namespace rsdw-system
kubectl -n rsdw-system create secret generic rsdw-c2-admin --from-literal=token=smoke-token
helm upgrade --install rsdw-c2 charts/rsdw-c2 \
  --namespace rsdw-system \
  --create-namespace \
  --set auth.adminTokenSecret.name=rsdw-c2-admin \
  --set image.repository=rsdw-c2 \
  --set image.tag=smoke \
  --set image.pullPolicy=Never \
  --set state.persistence.enabled=false
kubectl -n rsdw-system rollout status deployment/rsdw-c2-rsdw-c2 --timeout=120s
kubectl -n rsdw-system wait --for=condition=ready pod -l app.kubernetes.io/name=rsdw-c2 --timeout=120s
kubectl -n rsdw-system get service/rsdw-c2-rsdw-c2
kubectl -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 :8080 >"$test_dir/forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 50); do
  port=$(sed -n 's/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "$test_dir/forward.log")
  [[ -n "$port" ]] && break
  kill -0 "$forward_pid"
  sleep 0.2
done
[[ -n "$port" ]]
test "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/bootstrap")" = 401
curl -fsS -H 'Authorization: Bearer smoke-token' "http://127.0.0.1:$port/api/bootstrap" >"$test_dir/bootstrap.json"
grep -q '"servers":\[\]' "$test_dir/bootstrap.json"
curl -fsS "http://127.0.0.1:$port/" | grep -q DRAGONWILDS

kill "$forward_pid"
forward_pid=
kubectl -n rsdw-system set env deployment/rsdw-c2-rsdw-c2 RSDW_CHART=/tmp/fixture-chart RSDW_IMAGE_REPOSITORY=rsdw-c2
kubectl -n rsdw-system rollout status deployment/rsdw-c2-rsdw-c2 --timeout=120s
pod=$(kubectl -n rsdw-system get pod -l app.kubernetes.io/name=rsdw-c2 -o jsonpath='{.items[0].metadata.name}')
kubectl -n rsdw-system cp verification/fixture-chart "$pod:/tmp/fixture-chart"
kubectl -n rsdw-system exec "$pod" -- env KUBECONFIG=/tmp/rsdw-c2-kubeconfig kubectl auth can-i create pods/exec | grep -q yes
kubectl -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 :8080 >"$test_dir/fixture-forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 50); do
  port=$(sed -n 's/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "$test_dir/fixture-forward.log")
  [[ -n "$port" ]] && break
  kill -0 "$forward_pid"
  sleep 0.2
done
[[ -n "$port" ]]
api="http://127.0.0.1:$port/api"
curl -fsS -H 'Authorization: Bearer smoke-token' -H 'Content-Type: application/json' \
  -d '{"name":"Lifecycle fixture","namespace":"fixture-worlds","ownerId":"fixture-owner","imageTag":"smoke","maxPlayers":12}' "$api/servers" >"$test_dir/create.json"
kubectl -n fixture-worlds rollout status deployment/lifecycle-fixture-rsdragonwilds --timeout=120s
test "$(kubectl -n fixture-worlds get deployment lifecycle-fixture-rsdragonwilds -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" = '-ini:Game:[/Script/Engine.GameSession]:MaxPlayers=12'
curl -fsS -H 'Authorization: Bearer smoke-token' "$api/servers/lifecycle-fixture/logs?tail=10" | grep -q lifecycle-fixture-ready
curl -fsS -H 'Authorization: Bearer smoke-token' -X POST "$api/servers/lifecycle-fixture/actions/restart" >"$test_dir/restart.json"
kubectl -n fixture-worlds rollout status deployment/lifecycle-fixture-rsdragonwilds --timeout=120s
curl -fsS -H 'Authorization: Bearer smoke-token' -H 'Content-Type: application/json' \
  -d '{"imageTag":"smoke"}' "$api/servers/lifecycle-fixture/actions/update" >"$test_dir/update.json"
test "$(kubectl -n fixture-worlds get secret -l owner=helm,name=lifecycle-fixture --no-headers | wc -l)" -eq 2
curl -fsS -H 'Authorization: Bearer smoke-token' "$api/events" | grep -q 'Image update requested'

printf '%s\n' 'kind-smoke.sh passed'
