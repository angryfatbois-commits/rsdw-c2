#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
cluster_name="rsdw-c2-smoke-$$"
image_name=rsdw-c2:smoke
context="kind-$cluster_name"
export KIND_EXPERIMENTAL_PROVIDER=docker
kube=(kubectl --context "$context" --request-timeout=10s)
http=(curl --connect-timeout 5 --max-time 30)
command -v jq >/dev/null
command -v timeout >/dev/null

if ! timeout 15s docker info >/dev/null 2>&1; then
  printf '%s\n' 'INCONCLUSIVE: Docker access is required for kind-smoke.sh.' >&2
  exit 2
fi

test_dir=$(mktemp -d -t rsdw-c2-kind.XXXXXX)
export KUBECONFIG="$test_dir/kubeconfig"
cluster_created=false
forward_pid=
cleanup() {
  if "$cluster_created"; then
    "${kube[@]}" get pods -A -o wide >"$test_dir/pods.txt" 2>&1 || true
    "${kube[@]}" get events -A --sort-by=.lastTimestamp >"$test_dir/events.txt" 2>&1 || true
  fi
  if [[ -n "$forward_pid" ]]; then kill "$forward_pid" 2>/dev/null || true; fi
  if "$cluster_created"; then timeout 90s kind delete cluster --name "$cluster_name"; fi
  printf 'Test logs: %s\n' "$test_dir"
}
trap cleanup EXIT

timeout 600s docker build -t "$image_name" .
timeout 120s docker build -f verification/Dockerfile.fixture -t rsdw-c2-fixture:smoke .
existing=$(timeout 15s docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster_name" --format '{{.Names}}')
[[ -z "$existing" ]] || { printf '%s\n' 'Refusing to reuse an existing smoke cluster.' >&2; exit 1; }
cluster_created=true
bash scripts/kind-up.sh "$cluster_name"
timeout 20s kind export kubeconfig --name "$cluster_name" --kubeconfig "$KUBECONFIG"
timeout 180s kind load docker-image "$image_name" --name "$cluster_name"
timeout 180s kind load docker-image rsdw-c2-fixture:smoke --name "$cluster_name"
"${kube[@]}" create namespace rsdw-system
"${kube[@]}" -n rsdw-system create secret generic rsdw-c2-admin --from-literal=token=smoke-token
timeout 180s helm upgrade --install rsdw-c2 charts/rsdw-c2 --kube-context "$context" \
  --namespace rsdw-system \
  --create-namespace \
  --set auth.adminTokenSecret.name=rsdw-c2-admin \
  --set image.repository=rsdw-c2 \
  --set image.tag=smoke \
  --set image.pullPolicy=Never \
  --set state.persistence.enabled=false
"${kube[@]}" -n rsdw-system rollout status deployment/rsdw-c2-rsdw-c2 --timeout=120s
"${kube[@]}" -n rsdw-system wait --for=condition=ready pod -l app.kubernetes.io/name=rsdw-c2 --timeout=120s
"${kube[@]}" -n rsdw-system get service/rsdw-c2-rsdw-c2
"${kube[@]}" -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 :8080 >"$test_dir/forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 50); do
  port=$(sed -n 's/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "$test_dir/forward.log")
  [[ -n "$port" ]] && break
  kill -0 "$forward_pid"
  sleep 0.2
done
[[ -n "$port" ]]
test "$("${http[@]}" -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/api/bootstrap")" = 401
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' "http://127.0.0.1:$port/api/bootstrap" >"$test_dir/bootstrap.json"
grep -q '"servers":\[\]' "$test_dir/bootstrap.json"
"${http[@]}" -fsS "http://127.0.0.1:$port/" | grep -q DRAGONWILDS

kill "$forward_pid"
forward_pid=
"${kube[@]}" -n rsdw-system set env deployment/rsdw-c2-rsdw-c2 RSDW_CHART=/tmp/fixture-chart RSDW_IMAGE_REPOSITORY=rsdw-c2-fixture
"${kube[@]}" -n rsdw-system rollout status deployment/rsdw-c2-rsdw-c2 --timeout=120s
pod=$("${kube[@]}" -n rsdw-system get pod -l app.kubernetes.io/name=rsdw-c2 -o json | jq -er '[.items[] | select(.metadata.deletionTimestamp == null and .status.phase == "Running") | select(any(.status.conditions[]; .type == "Ready" and .status == "True"))] | if length == 1 then .[0].metadata.name else error("Expected one ready C2 pod") end')
timeout 30s "${kube[@]}" -n rsdw-system cp verification/fixture-chart "$pod:/tmp/fixture-chart"
timeout 30s "${kube[@]}" -n rsdw-system exec "$pod" -- env KUBECONFIG=/tmp/rsdw-c2-kubeconfig kubectl --request-timeout=10s auth can-i create pods/exec | grep -q yes
sa=system:serviceaccount:rsdw-system:rsdw-c2-rsdw-c2
for verb in get list; do
  [[ $("${kube[@]}" auth can-i "$verb" pods.metrics.k8s.io --as "$sa") == yes ]]
  [[ $("${kube[@]}" auth can-i "$verb" nodes.metrics.k8s.io --as "$sa" || true) == no ]]
done
for resource in nodes/proxy nodes/stats; do
  [[ $("${kube[@]}" auth can-i get "$resource" --as "$sa" || true) == no ]]
done
"${kube[@]}" -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 :8080 >"$test_dir/fixture-forward.log" 2>&1 &
forward_pid=$!
for _ in $(seq 1 50); do
  port=$(sed -n 's/Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "$test_dir/fixture-forward.log")
  [[ -n "$port" ]] && break
  kill -0 "$forward_pid"
  sleep 0.2
done
[[ -n "$port" ]]
api="http://127.0.0.1:$port/api"
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' -H 'Content-Type: application/json' \
  -d '{"name":"Lifecycle fixture","namespace":"fixture-worlds","ownerId":"0123456789abcdef0123456789abcdef","imageTag":"smoke","maxPlayers":12}' "$api/servers" >"$test_dir/create.json"
"${kube[@]}" -n fixture-worlds rollout status deployment/lifecycle-fixture-rsdragonwilds --timeout=120s
test "$("${kube[@]}" -n fixture-worlds get deployment lifecycle-fixture-rsdragonwilds -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" = '-ini:Game:[/Script/Engine.GameSession]:MaxPlayers=12'
fixture_pod=$("${kube[@]}" -n fixture-worlds get pod -l fixture=lifecycle-fixture -o jsonpath='{.items[0].metadata.name}')
metrics_ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if "${kube[@]}" --as "$sa" get --raw="/apis/metrics.k8s.io/v1beta1/namespaces/fixture-worlds/pods/$fixture_pod" >"$test_dir/pod-metrics.json" &&
    jq -e --arg pod "$fixture_pod" '
      .kind == "PodMetrics" and .metadata.name == $pod and
      (.timestamp | length > 0) and (.window | length > 0) and
      (.containers | length > 0) and
      all(.containers[];
        (.usage.cpu | test("^[0-9]+([.][0-9]+)?(n|u|m)?$")) and
        (.usage.memory | test("^[0-9]+([.][0-9]+)?([KMGTPE]i?|[numk])?$")) and
        (.usage.memory | capture("^(?<value>[0-9]+([.][0-9]+)?)").value | tonumber > 0))
    ' "$test_dir/pod-metrics.json" >/dev/null; then metrics_ready=true; break; fi
  sleep 2
done
"$metrics_ready" || { printf '%s\n' 'Fixture pod has no real CPU and memory metrics.' >&2; exit 1; }
"${kube[@]}" get --raw=/apis/metrics.k8s.io/v1beta1/nodes >"$test_dir/node-metrics.json"
jq -e '.items | length > 0 and all(.[];
  (.timestamp | length > 0) and (.window | length > 0) and
  (.usage.cpu | test("^[0-9]+([.][0-9]+)?(n|u|m)?$")) and
  (.usage.memory | test("^[0-9]+([.][0-9]+)?([KMGTPE]i?|[numk])?$")) and
  (.usage.memory | capture("^(?<value>[0-9]+([.][0-9]+)?)").value | tonumber > 0))
' "$test_dir/node-metrics.json" >/dev/null
telemetry_ready=false
for ((attempt=0; attempt<45; attempt++)); do
  "${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' "$api/servers/lifecycle-fixture/telemetry?range=5m" >"$test_dir/telemetry.json"
  if jq -e '
    .metrics.players.value == 2 and .metrics.players.status == "available" and
    .metrics.cpuCores.status == "available" and .metrics.cpuCores.value >= 0 and
    .metrics.memoryUsedBytes.value > 0 and .metrics.memoryLimitBytes.value == 33554432 and
    .metrics.cpuLimitCores.value == 0.05 and
    .metrics.diskCapacityBytes.value > 0 and
    .metrics.inboundBytesPerSecond.status == "available" and
    .metrics.outboundBytesPerSecond.status == "available" and
    .metrics.tickRate.status == "unsupported" and .metrics.tickRate.value == null and
    ([.samples[] | select(.players == 2 and .memoryUsedBytes > 0)] | length >= 2)
  ' "$test_dir/telemetry.json" >/dev/null; then telemetry_ready=true; break; fi
  sleep 2
done
"$telemetry_ready" || { printf '%s\n' 'C2 did not collect real resource metrics and fixture game observations.' >&2; jq '.metrics' "$test_dir/telemetry.json"; exit 1; }
printf 'Telemetry evidence: %s/telemetry.json\n' "$test_dir"
review_seconds=${RSDW_SMOKE_REVIEW_SECONDS:-0}
[[ "$review_seconds" =~ ^[0-9]+$ && "$review_seconds" -le 180 ]] || exit 2
printf 'Fixture dashboard for browser verification: http://127.0.0.1:%s/\n' "$port"
sleep "$review_seconds"
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' "$api/servers/lifecycle-fixture/logs?tail=10" | grep -q lifecycle-fixture-ready
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' -X POST "$api/servers/lifecycle-fixture/actions/restart" >"$test_dir/restart.json"
"${kube[@]}" -n fixture-worlds rollout status deployment/lifecycle-fixture-rsdragonwilds --timeout=120s
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' -H 'Content-Type: application/json' \
  -d '{"imageTag":"smoke"}' "$api/servers/lifecycle-fixture/actions/update" >"$test_dir/update.json"
test "$("${kube[@]}" -n fixture-worlds get secret -l owner=helm,name=lifecycle-fixture --no-headers | wc -l)" -eq 2
"${http[@]}" -fsS -H 'Authorization: Bearer smoke-token' "$api/events" | grep -q 'Image update requested'

printf '%s\n' 'kind-smoke.sh passed'
