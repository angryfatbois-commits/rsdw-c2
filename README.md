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

The ServiceAccount can create and update the resources that the Dragonwilds chart uses. The chart refuses to install without the admin token Secret. Install the console in a cluster or namespace reserved for these servers. Review the rendered `ClusterRole` before using it in a shared cluster.

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

The checks cover Go tests, the built service, the demo API, the embedded UI, and the control-plane chart. The verifier uses `helm template` for the game chart path. It does not download or start the game image.

Run the disposable low-memory cluster check when Docker access is available.

```sh
bash scripts/kind-smoke.sh
```

The script creates a disposable admin Secret inside the test cluster, deletes only the kind cluster that it creates, and exits with status 2 with `INCONCLUSIVE` when the Docker socket is unavailable.

The architecture and first-draft limits are in [ARCHITECTURE.md](ARCHITECTURE.md).
