# Live kind settings verification

Historical create-form verification, recorded before the user deployed their game server. This does not describe current runtime state. The live cluster is now stopped; see [telemetry verification](telemetry-verification.md) for the latest checks and restart instructions.

Verified 2026-09-16 against the local `rsdw-c2-live` kind cluster.

- Dashboard: http://127.0.0.1:18083/ (Kubernetes mode, not demo).
- Kubeconfig: `/tmp/rsdw-c2-live.kubeconfig`. Do not use the default cluster context.
- Namespace/release: `rsdw-system` / `rsdw-c2`.
- Dashboard image: `rsdw-c2:live-settings`; Pod Running, PVC Bound.
- Node ceiling: 3 GiB RAM and 2 CPU cores. This protects the laptop but does not guarantee the game can start.
- Port forward: user service `rsdw-c2-live-preview`.

The create form exposes owner EOS ID, server/world names, player count, image tag, namespace, region, memory and CPU limits, UDP port, storage size, service exposure, server/admin passwords, administrator IDs, logging, file validation, automatic stop on update, and additional startup arguments. API authentication is configured automatically. Password values use Kubernetes Secrets and are excluded from persisted dashboard state and API responses.

Passed `scripts/verify.sh`, `go test -race -p 1 ./...`, JavaScript syntax checks, and `git diff --check`. Backend tests cover settings-to-Helm mapping, resource persistence, bounds, and password privacy. In the live browser, required inputs blocked an empty submission; memory 128 MiB and CPU 50 millicores produced native validation errors; all four startup/exposure selects accepted changes. Defaults were restored and the form left open. No browser console errors were recorded.

The real game chart 0.1.1 was accessible and rendered with custom settings. No actual game server was created: an owner EOS ID was not provided. Game startup, player-limit enforcement, joining over UDP, and real runtime metrics remain unverified. NodePort on kind does not automatically map the game port to localhost.
