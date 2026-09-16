#!/usr/bin/env bash
set -euo pipefail

cluster_name=${1:?Usage: install-metrics-server.sh CLUSTER_NAME}
[[ $# == 1 && "$cluster_name" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || exit 2
export KIND_EXPERIMENTAL_PROVIDER=docker
for command in docker kind kubectl timeout curl jq; do command -v "$command" >/dev/null; done
node="${cluster_name}-control-plane"
nodes=$(timeout 15s docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster_name" --format '{{.Names}}')
[[ "$nodes" == "$node" ]] || { printf '%s\n' 'Expected one local kind control-plane node.' >&2; exit 1; }
[[ $(timeout 15s docker inspect --format '{{.State.Running}}' "$node") == true ]] || {
  printf '%s\n' 'The local kind node must already be running.' >&2
  exit 1
}
test_dir=$(mktemp -d -t rsdw-metrics.XXXXXX)
trap 'rm -f "$test_dir/kubeconfig" "$test_dir/components.yaml"; rmdir "$test_dir"' EXIT
timeout 20s kind export kubeconfig --name "$cluster_name" --kubeconfig "$test_dir/kubeconfig"
kube=(kubectl --kubeconfig "$test_dir/kubeconfig" --context "kind-$cluster_name" --request-timeout=10s)
"${kube[@]}" get --raw=/readyz >/dev/null
minor=$("${kube[@]}" get --raw=/version | jq -er '.minor | capture("^(?<minor>[0-9]+)").minor | tonumber')
if ((minor >= 34)); then
  version=v0.9.0
elif ((minor >= 31)); then
  version=v0.8.1
else
  printf '%s\n' 'Metrics Server requires Kubernetes 1.31 or newer in this workflow. Existing cluster was retained.' >&2
  exit 1
fi
curl --fail --silent --show-error --location --connect-timeout 10 --max-time 60 \
  "https://github.com/kubernetes-sigs/metrics-server/releases/download/$version/components.yaml" \
  --output "$test_dir/components.yaml"
# kind kubelets use certificates that the Metrics Server cannot verify.
sed -i '/^        - --metric-resolution=15s$/a\        - --kubelet-insecure-tls' "$test_dir/components.yaml"
grep -q '^        - --kubelet-insecure-tls$' "$test_dir/components.yaml"
"${kube[@]}" apply -f "$test_dir/components.yaml"
"${kube[@]}" -n kube-system rollout status deployment/metrics-server --timeout=180s
"${kube[@]}" wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io --timeout=120s
"${kube[@]}" get --raw=/apis/metrics.k8s.io/v1beta1/nodes | jq -e '.items | length > 0' >/dev/null
printf 'Metrics Server %s is available in kind-%s.\n' "$version" "$cluster_name"
