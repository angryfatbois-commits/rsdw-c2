# RSDW C2

RSDW C2 is a Kubernetes command and control console for RuneScape Dragonwilds dedicated servers.

The first draft covers four jobs:

- Create a server with the `rsdragonwilds-helm` chart.
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

The ServiceAccount can create and update the resources that the Dragonwilds chart uses. Token mode requires the admin token Secret. Install the console in a cluster or namespace reserved for these servers. Review the rendered `ClusterRole` before using it in a shared cluster.

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

Run the disposable low-memory cluster check when Docker access is available.

```sh
bash scripts/kind-smoke.sh
```

The script creates a disposable admin Secret inside the test cluster, deletes only the kind cluster that it creates, and exits with status 2 with `INCONCLUSIVE` when the Docker socket is unavailable.

The kind test uses a private kubeconfig. It verifies authentication, empty inventory, and create, logs, restart, and update operations against a small Helm fixture. It does not download or start the game image. The Max players field passes the chart's documented Unreal override; the game build determines whether that override is enforced.

See the [mockup refinement validation record](verification/mockup-refinement.md) for browser results and test limits. Remaining live-game telemetry and rollout-state gaps are tracked in [issue #1](https://github.com/petzkod5/rsdw-c2/issues/1).

The architecture and first-draft limits are in [ARCHITECTURE.md](ARCHITECTURE.md).
