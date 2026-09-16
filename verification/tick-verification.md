# Issue 3 verification

Verified on September 16, 2026. The producer is a source patch in the companion `rsdragonwilds-helm` change. The dashboard change consumes its authenticated endpoint. Both are needed before a deployed server can supply these metrics.

## Actual game

An isolated container ran the retained Dragonwilds executable with build ID `3b4ce30aed886594`. Installed game files were mounted read-only. A separate writable Saved directory kept the user's world untouched. The process had no network, one CPU, 2304 MiB memory, and 2816 MiB combined memory/swap. No Unreal rebuild or game-file edit was performed.

[The capture](tick-live-observations.json) contains 13 actual API windows across 60.997 seconds. The independently read native frame counter advanced 1822 times, or 29.8703 Hz. The last API window reported 29.8940 Hz, 299 completed ticks, and 10.0020 seconds. Its execution-duration percentiles were 0.459720 ms p50, 0.729415 ms p95, and 0.888823 ms p99.

`check-tick-evidence.py` compares overlapping native-counter and API windows and rejects a stuck counter or doubled rate. Thirty HTTP reads did not stop tick progression. The native counter advanced 46 times during that 1.541-second burst. An unauthenticated request returned HTTP 401. Health reported an initialized engine with zero players.

[The restart capture](tick-live-restart.json) shows the API uptime reset. Thirty-three warming responses contained null measurements before a new measured window became ready. Native tests separately exercise stale state, long completed ticks, storage overflow and recovery, argument forwarding, exception propagation, unknown-build rejection, concurrency, and JSON states.

The duration boundary was verified from the executable and actual engine instance, then measured by monotonic clocks around its original call. It is `UDomGameEngine::Tick`, not the entire outer frame loop. There was no second live duration profiler. This was an empty offline world, not a player-load benchmark or externally joinable session.

## Consumer and browser

Go tests validate the actual captured windows through the production decoder. Additional tests reject absent, malformed, stale, wrong-scope, incoherent, and impossible percentile snapshots. Pod replacement invalidates all tick readings. The producer has a two-second freshness limit; accepted cached observations retain the existing C2 45-second expiry and original timestamps.

The browser used the production UI with `node verification/tick-preview.cjs`, which replays the recorded actual measurements and rejects mutations. It was labelled as recorded data. This check was not a live Kubernetes connection. The browser showed 29.9 TPS and the recorded p50/p95/p99 values. Both duration series, distinct line styles, keyboard-accessible data disclosure, 60-second/5-minute/hour ranges, and pause state were verified. No browser errors were reported.

CSV clicks saved `rsdw-telemetry (5).csv` and `rsdw-telemetry (6).csv` in the local Downloads directory. The browser automation's download event timed out, but the saved files and their contents verified completion. Exports contain measured values, source timestamps, units, and source labels. The temporary preview and browser tab were closed afterward.

## Build and Kubernetes

`scripts/verify.sh` passed Go tests, vet, JavaScript tests, evidence checks, build, Helm lint/render, and HTTP checks. `GOMAXPROCS=2 go test -race ./...` also passed. The server image built successfully in the Sniper SDK and ran native tests. Its final dynamic-library dependency check passed. Local image `rsdw-tick:verify` has ID `sha256:eacb2b4443dc1496a9f0ee64f5cb8486ad052928801f608dfeb96d99d88e561d`. No image or release tag was published.

`scripts/kind-smoke.sh` passed with Metrics Server on a disposable resource-limited cluster. It checked real CPU/memory metrics, resource limits, RBAC denials, logs, restart, and image update. Game responses in that test were fixtures and intentionally lacked the new endpoint, proving compatibility with older API images. Actual tick measurements were verified separately above.

Two test-harness failures were corrected. Docker now copies the captured regression fixture before running Go tests. The kind test selects the ready replacement pod instead of a terminating pod after rollout. Final smoke logs are in `/tmp/rsdw-c2-kind.N02sFh`. The disposable cluster was deleted. Docker showed no running containers, and the original `rsdw-c2-live-control-plane` and measured game container remained stopped. Saved world storage was preserved.

## Review decisions

Independent review found two correctness gaps. C2 now rejects impossible sample counts, subsecond ready windows, durations longer than their window, and unequal values at coincident percentile ranks. The live evidence checker now asserts agreement with the independent native counter instead of only recording it. Both fixes have negative tests.

The comment pass removed 14 redundant comment lines and two script docstrings. Public contract comments and the deliberate bounded-storage limit remain. Additional abstraction solely to encode explanatory comments was rejected. The final implementation uses the existing API, history store, and chart renderer without a new runtime service or UI dependency.

Prove It Works changed the hook target after the actual engine exposed its game-specific override. Model the Domain kept unavailable states separate from coherent numeric snapshots. UI/UX Pro Max led to distinct series styles and accessible numeric tables using the existing chart component.
