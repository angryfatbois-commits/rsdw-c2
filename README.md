# RSDW C2

RSDW C2 is a Kubernetes command and control console for RuneScape Dragonwilds dedicated servers.

The console supports these operations:

- Create a server with the `rsdragonwilds-helm` chart.
- Save named player IDs and select one when creating a server.
- Read live server health, player count, uptime, and logs; show resource metrics when a metrics source is available.
- Restart a server after confirmation.
- Detect image drift and push a new image tag after confirmation.

Backups and integrations are not part of this release.

## Run the console locally

Use demo mode to run the console without a Kubernetes cluster.

```sh
RSDW_DEMO_DATA=true RSDW_STATE_FILE=./state.json go run .
```

Open `http://localhost:8080`.

Demo mode loads one sample server so you can exercise the dashboard. A normal start uses an empty state file.

## Save player IDs

Open **Saved IDs** in the console to add, edit, or delete a named player ID. Copy your Player ID from the game's Settings menu, as described in the [official dedicated-server guide](https://dragonwilds.runescape.com/news/how-to-dedicated-servers).

In **Create a server**, choose a saved ID to populate **Owner EOS player ID**. You can edit that field or enter a one-off ID manually. The selector shows both the display name and player ID, so names can repeat.

Saved IDs and manual owner IDs must contain exactly 32 ASCII hexadecimal characters, `0-9`, `a-f`, or `A-F`. C2 converts uppercase hexadecimal letters to lowercase. It rejects empty values, whitespace, separators, and other characters without stripping them. This checks the identifier format, not whether the account exists. The same rules apply in demo mode.

Each canonical player ID can be saved once. Adding a duplicate or editing a record to use another record's player ID returns `409 Conflict`. Editing a record without changing its player ID is allowed. Deleting a record frees its player ID for reuse.

C2 stores records as `users` in its state file, keyed by stable record ID. Older state files load with an empty collection and retain their servers and events. Editing or deleting a saved ID never changes the owner ID already copied into a server.

Saved IDs require admin access. OIDC admins use their session cookie and the existing CSRF protection for changes. Viewers cannot list or change saved IDs. Token mode uses the existing admin bearer token, including explicitly configured demo tokens. Demo mode without a token allows local access.

| Method | Path | Body | Success |
| --- | --- | --- | --- |
| GET | `/api/users` | None | `200`, `{"users":[{"id":"...","name":"Alice","playerId":"..."}]}` |
| POST | `/api/users` | `{"name":"Alice","playerId":"0123456789abcdef0123456789abcdef"}` | `201`, saved record |
| PUT | `/api/users/{id}` | Both `name` and `playerId` | `200`, updated record with the same `id` |
| DELETE | `/api/users/{id}` | None | `204`, no body |

Names are trimmed and must be a single line of 1 to 48 bytes. Invalid input returns `400`; missing records return `404`. Player IDs are identifiers, not passwords or login credentials. These operations do not add player IDs to logs or events.

## Deploy the console

Build and publish the image, then create the required admin token Secret.

```sh
docker build -t ghcr.io/petzkod5/rsdw-c2:0.1.0 .
kubectl create namespace rsdw-system
kubectl -n rsdw-system create secret generic rsdw-c2-admin \
  --from-literal=token="$(openssl rand -hex 32)"
helm upgrade --install rsdw-c2 charts/rsdw-c2 \
  --namespace rsdw-system \
  --set image.tag=0.1.0 \
  --set auth.adminTokenSecret.name=rsdw-c2-admin
```

The Service is a `ClusterIP`. Use a port-forward or put it behind your existing authentication proxy.

```sh
kubectl -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 8080:8080
```

To create an Ingress, add these values to a file and pass it to Helm with `-f`.

```yaml
ingress:
  enabled: true
  ingressClassName: nginx
  annotations: {}
  hosts:
    - c2.example.com
  paths:
    - path: /
      pathType: Prefix
  tls:
    - secretName: rsdw-c2-tls
      hosts:
        - c2.example.com
```

The TLS Secret must contain `tls.crt` and `tls.key` in the release namespace. The UI uses root-relative asset and API URLs, so serve it at `/`. Subpath prefixes such as `/c2` are not supported.

The ServiceAccount can create and update the resources that the Dragonwilds chart uses. Token mode requires the admin token Secret. Install the console in a cluster or namespace reserved for these servers. Review the rendered `ClusterRole` before using it in a shared cluster.

If an authentication proxy sits in front of the Ingress in token mode, configure it to forward the browser's `Authorization: Bearer <admin-token>` header unchanged. RSDW C2 validates that bearer token against the admin token Secret.

For identity-provider sign in with admin and viewer roles, follow [Configure OIDC sign in](docs/oidc.md). OIDC mode requires HTTPS, an existing client Secret, and explicit role assignments. Its callback is `/api/auth/callback`. Viewers can read dashboard and telemetry data; admins can also read operational data and manage servers. OIDC mode does not accept the shared admin token or forwarded identity headers.

## Configure the server chart

The console uses these defaults.

- Chart: `oci://ghcr.io/petzkod5/charts/rsdragonwilds`.
- Chart version: `0.1.1`.
- Image repository: `ghcr.io/petzkod5/rsdragonwilds-server`.
- Namespace: `dragonwilds`.

Set `RSDW_CHART`, `RSDW_CHART_VERSION`, and `RSDW_IMAGE_REPOSITORY` on the console Deployment when the chart or image lives somewhere else.

The source chart requires an EOS owner ID and an existing API-token Secret. RSDW C2 creates the namespace and a release-specific Secret before it runs `helm upgrade --install`.

## Verify the project

Run the deterministic checks.

```sh
bash scripts/verify.sh
```

The checks cover Go tests, signed-token OIDC flows and rejection cases, the built service, the demo API, UI capability and stale-response tests, CSV tests, and positive and negative chart renders. Each run uses fresh state and automatically assigned ports. Run `go test -race ./...` for race checks. The [local OIDC browser fixture](docs/oidc.md#run-the-local-browser-fixture) supports independent browser testing without a cluster.

With Playwright and Chromium available, run `node tests/users-browser.cjs` after `bash scripts/verify.sh` for the authenticated Saved IDs browser flow. Set `NODE_PATH` if Playwright is installed outside the project, and `RSDW_TEST_CHROMIUM` to use a specific Chromium executable. The test uses temporary state and demo mode without a Kubernetes cluster.

Run the disposable low-memory cluster check when Docker access is available.

```sh
bash scripts/kind-smoke.sh
```

The script creates a disposable admin Secret inside the test cluster, deletes only the kind cluster that it creates, and exits with status 2 with `INCONCLUSIVE` when the Docker socket is unavailable.

The kind test uses a private kubeconfig. It verifies authentication, empty inventory, and create, logs, restart, and update operations against a small Helm fixture. It does not download or start the game image. The Max players field passes the chart's documented Unreal override; the game build determines whether that override is enforced.

See the [mockup refinement validation record](verification/mockup-refinement.md) for browser results and test limits. Remaining live-game telemetry and rollout-state gaps are tracked in [issue #1](https://github.com/petzkod5/rsdw-c2/issues/1).

The architecture and first-draft limits are in [ARCHITECTURE.md](ARCHITECTURE.md).
