#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
chart=(charts/rsdw-c2 --set auth.adminTokenSecret.name=console-admin)
check_env() {
  local rendered="$1" name="$2" value="$3"
  [[ "$rendered" == *"name: $name"$'\n'"              value: \"$value\""* ]]
}
rendered=$(helm template pressure-test "${chart[@]}")
check_env "$rendered" RSDW_MEMORY_PRESSURE_ENABLED false
check_env "$rendered" RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT 85
check_env "$rendered" RSDW_MEMORY_PRESSURE_DURATION 10m
rendered=$(helm template pressure-test "${chart[@]}" --set memoryPressure.enabled=true --set-json memoryPressure.thresholdPercent=90.5 --set memoryPressure.duration=1m30s)
check_env "$rendered" RSDW_MEMORY_PRESSURE_ENABLED true
check_env "$rendered" RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT 90.5
check_env "$rendered" RSDW_MEMORY_PRESSURE_DURATION 1m30s
for invalid in enabled=maybe thresholdPercent=-1 thresholdPercent=101 duration=0s duration=0h0m0.0s duration=-1m duration=10 duration=bad; do
  if helm template pressure-test "${chart[@]}" --set "memoryPressure.$invalid" >/dev/null 2>&1; then
    printf 'Unexpected chart success: %s\n' "$invalid" >&2
    exit 1
  fi
done
helm lint "${chart[@]}"
printf '%s\n' 'Memory pressure chart checks passed'
