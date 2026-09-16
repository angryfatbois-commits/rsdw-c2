#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root_dir"
cluster_name=rsdw-c2-smoke
image_name=rsdw-c2:smoke

if ! docker info >/dev/null 2>&1; then
  printf '%s\n' 'INCONCLUSIVE: Docker access is required for kind-smoke.sh.' >&2
  exit 2
fi

cleanup() { kind delete cluster --name "$cluster_name" >/dev/null 2>&1 || true; }
trap cleanup EXIT

kind create cluster --name "$cluster_name" --wait 60s
docker build -t "$image_name" .
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

printf '%s\n' 'kind-smoke.sh passed'
