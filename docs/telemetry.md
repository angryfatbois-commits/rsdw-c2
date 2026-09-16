# Telemetry sources and limits

The dashboard schedules collection every 15 seconds, even when no browser is open. Four workers bound collection concurrency; slow sources or larger fleets can lengthen that interval. It keeps up to one hour of observations in memory. A dashboard service restart clears history. Collection failures create gaps in the metric map and charts, not invented replacement values.

| Measurement | Source | Meaning |
| --- | --- | --- |
| Players | Authenticated game `/api/players` | API-reported connected count. The current upstream API can return an empty roster after an internal lookup failure. |
| Engine readiness | Game `/api/health` | Whether the mod reports that the engine is ready. |
| API uptime | Game `/api/health` | Mod process uptime, not a world availability percentage. |
| CPU cores | Kubernetes Metrics API | Average CPU use of the `server` container over the source's measurement window. |
| CPU limit used | CPU cores and the running Pod's CPU limit | Percentage of the container's configured ceiling. Without a limit, cores remain available but the percentage does not. |
| Memory working set | Kubernetes Metrics API | The `server` container's working-set bytes. Sidecar memory is excluded. |
| Memory limit | Running Pod specification | The actual container memory ceiling, not the form's requested value. |
| Pod traffic | Non-loopback `/proc/net/dev` counters | Received and transmitted bytes per second across the Pod network. Includes sidecar traffic and excludes loopback. Two readings are required. Pod changes, counter resets, and collection gaps restart the baseline. |
| Data filesystem | `df` at `/home/steam/rsdw-dedicated` | Used bytes and capacity of the filesystem backing the data mount. On kind, this may be a shared filesystem. It is not world-save size or a PVC quota. |
| Tick rate | No supported source | The current game API does not expose observed ticks per second. This stays unsupported, with no numeric value. |

Each metric has its own status, source, observation timestamp, and failure reason. A missing Metrics API does not suppress player data. A real zero is displayed as zero. An expired observation is stale and is not presented as current.

Live server responses omit the old flat numeric metric fields. API clients must read the nullable `metrics` map. `metricsAvailable` means at least one reading is available, not that every metric is available. Demo records without a metric map retain their legacy fields.

Charts use observation timestamps and preserve gaps. Their expandable tables provide the numeric data without requiring visual chart inspection. CSV export includes metric names, values, source timestamps, units, sources, and availability.

Metrics Server provides CPU and memory, not historical storage, network throughput, disk usage, or game ticks. The kind startup script installs a pinned Metrics Server release. See [Kubernetes resource metrics](https://kubernetes.io/docs/tasks/debug/debug-cluster/resource-metrics-pipeline/) and [Metrics Server requirements](https://kubernetes-sigs.github.io/metrics-server/).

Observed tick rate requires additional game-engine instrumentation. The current producer exposes health, player, and administration routes, but no observed tick-rate metric. See [RSDWServerAPI routes](https://github.com/dkoz/RSDWServerAPI/blob/main/src/api/api_routes.cpp) and [system routes](https://github.com/dkoz/RSDWServerAPI/blob/main/src/api/system_routes.cpp). Configured frame limits, CPU consumption, and readiness are not substitutes for measured ticks.

Track producer instrumentation in [issue #3](https://github.com/petzkod5/rsdw-c2/issues/3).
