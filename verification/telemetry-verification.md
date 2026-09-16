# Telemetry verification

Run date: September 16, 2026. Tests used a disposable kind cluster. The user's `rsdw-c2-live` cluster stayed stopped throughout implementation and verification.

## Automated checks

- `scripts/verify.sh` passed Go tests, Go vet, JavaScript syntax and rendering checks, Helm lint and render checks, and local HTTP checks.
- `GOMAXPROCS=2 go test -race -count=1 -p 1 ./...` passed after the backend review fixes.
- Shell syntax and `git diff --check` passed.
- The container build ran its Go tests and produced the production image.
- `scripts/kind-smoke.sh` passed on Kubernetes v1.36.1 with Metrics Server v0.9.0 and a 3 GiB, 2 CPU node cap. C2 deployed the fixture via Helm, collected telemetry, read logs, restarted the fixture, and submitted an image update. The test deleted its disposable cluster afterward.

The failed first run and review findings are recorded in [telemetry-review.md](telemetry-review.md).

## Observed values

The successful run retained `/tmp/rsdw-c2-kind.neUu57/telemetry.json` on the test laptop. It contained three history frames. A captured reading reported:

| Reading | Value | Evidence source |
| --- | --- | --- |
| CPU | 0.004597971 cores | Kubernetes Metrics API |
| CPU limit | 0.05 cores | Running Pod specification |
| CPU limit used | 9.195942% | CPU divided by the limit |
| Memory working set | 696,320 bytes | Kubernetes Metrics API |
| Memory limit | 33,554,432 bytes | Running Pod specification |
| Inbound traffic | 72.50654165442879 bytes/second | Pod interface counter deltas |
| Outbound traffic | 87.56254483950143 bytes/second | Pod interface counter deltas |
| Data filesystem capacity | 1,022,059,925,504 bytes | `df` at the fixture data mount |
| Players | 2 | Test-only game API response |
| API uptime | 120 seconds | Test-only game API response |
| Tick rate | null, unsupported | No producer |

The resource measurements came from the running Linux container and Kubernetes, not canned data. Player count, readiness, and uptime came from a deliberately small HTTP fixture. This run did not start or load-test Dragonwilds. The filesystem measurement is shared backing capacity, not a world's private storage quota.

## Browser checks

The browser showed a live Kubernetes connection, the fixture server online, players at 2/12, CPU percentages, working-set memory, filesystem capacity, bidirectional traffic, source timestamps, and an explicit unsupported tick-rate explanation. The page rendered without console errors.

The 60-second, 5-minute, and 1-hour selectors worked. The player-data disclosure stayed open across a manual refresh. Pause changed to Resume and displayed `Updates paused`; resuming restored live updates. Chart histories rendered from collected samples.

Export CSV saved `/home/petzko/Downloads/rsdw-telemetry (3).csv`. Direct file inspection confirmed the metric, value, source timestamp, status, reason, unit, and source columns. Tick-rate rows had empty numeric cells and `unsupported` status. The browser automation download notification timed out, but the saved file verified the result.

The first browser pass caught small CPU values rounded to zero and an awkward line break in the unsupported-value card. The final UI preserves three significant digits below one, scales the CPU chart below one core, and uses smaller text for long card values. Rendering regression tests cover fractional CPU values and the chart ceiling.

The final browser pass observed `0.00259 cores` and a CPU chart ceiling of `0.00352`. The unsupported-value card fit without splitting its word. CPU, memory, and network data disclosures remained open after refresh, with 8, 8, and 16 rows respectively. A second CSV download saved `rsdw-telemetry (4).csv`; direct inspection verified its header and source-timestamped values. Final-run evidence is under `/tmp/rsdw-c2-kind.lt3Ycs`.

## Limits and next start

Actual game-engine tick rate and tick-time percentiles remain unimplemented. [Issue #3](https://github.com/petzkod5/rsdw-c2/issues/3) tracks the producer instrumentation required to measure them. History is bounded to one hour and resets when C2 restarts.

The stopped live cluster still has its previous dashboard image. Follow [the existing-cluster upgrade steps](../docs/kind.md#load-a-changed-dashboard-into-an-existing-cluster) to resume it, install Metrics Server, and load this implementation. No world data was deleted.

Final cleanup passed. Both successful smoke clusters were deleted, `docker ps` returned no running containers, the live node reported `exited`, the preview service reported `inactive`, and neither 18081 nor 18083 had a listener. Calling `kind-down.sh rsdw-c2-live` again was safe and retained the stopped node.
