#!/usr/bin/env bash
set -euo pipefail

cluster_name=${1:?Usage: kind-down.sh CLUSTER_NAME}
[[ $# == 1 && "$cluster_name" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]] || exit 2
command -v timeout >/dev/null
timeout 15s docker info >/dev/null
nodes=$(timeout 15s docker ps -a --filter "label=io.x-k8s.kind.cluster=$cluster_name" --format '{{.Names}}')
if [[ -n "$nodes" ]]; then
  mapfile -t node_names <<<"$nodes"
  timeout 90s docker stop --timeout 60 "${node_names[@]}" >/dev/null
fi
printf '%s is stopped. Node containers, volumes, and worlds were retained.\n' "$cluster_name"
