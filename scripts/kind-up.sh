#!/usr/bin/env bash
set -euo pipefail

cluster_name=${1:?Usage: kind-up.sh CLUSTER_NAME}
[[ $# == 1 && "$cluster_name" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || exit 2
export KIND_EXPERIMENTAL_PROVIDER=docker
root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
for command in docker kind kubectl timeout curl jq; do command -v "$command" >/dev/null; done
timeout 15s docker info >/dev/null
test_dir=$(mktemp -d -t rsdw-kind-up.XXXXXX)
trap 'rm -f "$test_dir/kubeconfig"; rmdir "$test_dir"' EXIT
node="${cluster_name}-control-plane"
nodes=$(timeout 15s docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster_name" --format '{{.Names}}')
create_status=0
if [[ -z "$nodes" ]]; then
  timeout 300s kind create cluster --name "$cluster_name" --retain --wait 0s --kubeconfig "$test_dir/kubeconfig" || create_status=$?
elif [[ "$nodes" != "$node" ]]; then
  printf 'Refusing unsupported topology for %s. Expected one control-plane node.\n' "$cluster_name" >&2
  exit 1
fi
timeout 20s docker update --memory 3g --memory-swap 3g --cpus 2 "$node" >/dev/null
if ((create_status != 0)); then
  printf '%s\n' 'kind creation failed. Any retained node is capped; inspect it before retrying.' >&2
  exit "$create_status"
fi
state=$(timeout 15s docker inspect --format '{{.State.Status}}' "$node")
case "$state" in
  exited|created) timeout 30s docker start "$node" >/dev/null ;;
  running) ;;
  *) printf 'Cannot resume node in state %s. No data was deleted.\n' "$state" >&2; exit 1 ;;
esac
timeout 20s kind export kubeconfig --name "$cluster_name" --kubeconfig "$test_dir/kubeconfig"
kube=(kubectl --kubeconfig "$test_dir/kubeconfig" --context "kind-$cluster_name" --request-timeout=10s)
ready=false
for ((attempt=0; attempt<30; attempt++)); do
  if "${kube[@]}" get --raw=/readyz >/dev/null 2>&1; then ready=true; break; fi
  sleep 2
done
"$ready" || { printf '%s\n' 'Kubernetes API did not become ready. Node and world data were retained.' >&2; exit 1; }
"${kube[@]}" wait --for=condition=Ready nodes --all --timeout=120s
bash "$root_dir/scripts/install-metrics-server.sh" "$cluster_name"
printf 'kind-%s is ready. Node limit is 3 GiB and 2 CPUs.\n' "$cluster_name"
