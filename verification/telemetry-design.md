# Live telemetry design

The app schedules collection every 15 seconds independently of browser requests. Slow sources and fleets larger than four servers can lengthen the interval. It retains at most one hour in memory. Restarting the dashboard starts a new history, without fabricated backfill.

Candidate A keeps collection and bounded history in the Go process. Candidate B uses a separate Prometheus deployment for scraping and history. A is the base because short dashboard windows do not justify another daemon on this laptop. B's explicit source timestamps and partial-failure treatment are retained. Neither source design produces a real tick rate from the current game API.

The design review used separate inherited-model agents, not different models. Russell proposed A, Noether proposed B, and Hubble cross-reviewed both. Hubble preferred A for the current short history windows and laptop memory constraint. Long-term retention remains a reason to reconsider B.

The existing browser-driven Refresh mixes settings and observations. The new collector owns observations separately and joins them with persisted settings at read time. Collection must not overwrite a concurrent image or resource change.

The wire contract uses `server.metrics` and `telemetry.metrics`, maps keyed by metric name. Each reading contains `value` as a nullable number, `status` as available, unavailable, warming_up, unsupported, error, or stale, `source`, `unit`, `observedAt`, and `reason`. Metric keys are players, uptimeSeconds, engineReady, cpuCores, cpuPercent, cpuLimitCores, memoryUsedBytes, memoryLimitBytes, diskUsedBytes, diskCapacityBytes, diskPercent, inboundBytesPerSecond, outboundBytesPerSecond, networkBytesPerSecond, and tickRate. CPU percent is a percentage of the observed container CPU limit. Engine readiness is 0 or 1 from the API.

`telemetry.samples` is a chronological list of flat nullable metric values with `timestamp` and per-metric `observedAt` timestamps. Missing data is null. Charts use time coordinates and break lines on missing observations. CSV includes source time and availability rather than converting missing values to zero.

CPU and memory come from Metrics Server for the exact server container. Container limits come from the same Pod. Player count and API uptime come from authenticated game endpoints. Network rates use non-loopback pod interface counter deltas with reset and pod-replacement detection. Disk means the capacity and usage of the data mount's backing filesystem, not world-save size or requested PVC capacity. No node-proxy permission is needed.

The collector validates source payloads and timestamps, exposes individual source failures, bounds concurrency and history, and excludes stale observations from current numeric values. It reports only health checks actually performed. Tick rate remains unsupported until the game has verified instrumentation.

Verification covers real Metrics API samples, API-to-chart history, zeros versus missing values, source failure, stale data, counter resets, pod replacement, quantity units, and control-plane restart. The user's live kind cluster remains stopped. A disposable capped fixture cluster is the only live Kubernetes test target.
