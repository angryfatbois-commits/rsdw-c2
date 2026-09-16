# Run and stop a local kind cluster

Use these scripts with Docker, Bash, kind, kubectl, curl, jq, and GNU timeout. The smoke test also requires Helm.
Run commands from the repository root. Pass the cluster name explicitly.

## Start or resume a cluster

```sh
bash scripts/kind-up.sh rsdw-c2-dev
```

The script creates a single control-plane node if the named cluster is absent. It resumes a stopped node and accepts an already running node.
It refuses existing clusters with other node layouts. Failed startup retains the node so that you can inspect or retry it.
An unsupported Kubernetes version also leaves the cluster intact.

The Docker node receives a 3 GiB memory limit, no additional swap, and a 2 CPU quota.
kind does not expose node resource limits during creation. New nodes receive the cap immediately after `kind create cluster` returns, so initial bootstrap is not capped.
Existing stopped nodes receive the cap before they start. The cap limits consumption but does not reserve host capacity or limit world storage size.

Startup waits for the Kubernetes API, node readiness, Metrics Server rollout, and the metrics API.
Docker and kind operations have timeouts. Cluster creation allows 300 seconds, API polling allows 30 attempts with 10-second request limits, and node readiness allows 120 seconds.
Metrics Server download allows 60 seconds, rollout allows 180 seconds, and APIService readiness allows 120 seconds.
Failures exit nonzero and preserve node data. These are per-operation bounds, not a single overall deadline.

Every Kubernetes command uses `kind-<name>` and a temporary kubeconfig exported for that cluster.
The scripts do not use or change your current kubeconfig context. They force the Docker kind provider.
To run your own commands, export a kubeconfig explicitly.

```sh
kind export kubeconfig --name rsdw-c2-dev --kubeconfig /tmp/rsdw-c2-dev.kubeconfig
kubectl --kubeconfig /tmp/rsdw-c2-dev.kubeconfig --context kind-rsdw-c2-dev get nodes
```

## Stop a cluster and retain worlds

```sh
bash scripts/kind-down.sh rsdw-c2-dev
```

The script stops only Docker nodes with that exact kind cluster label. An absent or stopped cluster is safe to stop again.
Docker allows 60 seconds for shutdown before forcing termination. Application shutdown and world-save behavior still depend on the game.
The script does not delete containers, volumes, PVCs, Helm releases, or worlds. Run `kind-up.sh` with the same name to resume.
Keep C2 state persistence enabled and store game worlds on persistent volumes. Pod-local ephemeral files are not durable world storage.
Retained node storage is not a backup. Deleting the kind cluster or pruning its storage can destroy local worlds.

## Reinstall Metrics Server on local kind

```sh
bash scripts/install-metrics-server.sh rsdw-c2-dev
```

Startup invokes this installer automatically. Repeated installs apply the same pinned manifest with the same local patch.
The installer requires a running single-node kind cluster and exports its own kubeconfig before any Kubernetes write.
It never starts a stopped cluster.

The official latest release checked on September 16, 2026 is [Metrics Server v0.9.0](https://github.com/kubernetes-sigs/metrics-server/releases/tag/v0.9.0).
The [upstream compatibility matrix](https://github.com/kubernetes-sigs/metrics-server/blob/v0.9.0/README.md#compatibility-matrix) requires Kubernetes 1.34+ for v0.9.0.
The installer pins [v0.8.1](https://github.com/kubernetes-sigs/metrics-server/releases/tag/v0.8.1) for Kubernetes 1.31 through 1.33 and refuses older clusters.
Manifests come from the official versioned release URL, never the mutable `latest` URL.

The installer adds `--kubelet-insecure-tls` only to the local Metrics Server container.
This flag disables verification of the kubelet serving certificate on Metrics Server's HTTPS connections to kubelets. kind's kubelet certificates require this local exception.
It does not disable kubectl's API-server certificate verification or modify kubelet authentication or authorization.
The official manifest also sets `APIService.spec.insecureSkipTLSVerify: true` for the API-server-to-Metrics-Server connection. That is a separate upstream exception retained here.
Do not apply this local manifest or either TLS exception to production. Use trusted serving certificates and verified TLS for both connections in production.
The scripts do not patch production cluster settings.

C2's chart grants `get` and `list` only for pod resources in `metrics.k8s.io`.
It adds no core `nodes/stats` or `nodes/proxy` permission. Metrics Server's own upstream role includes `nodes/metrics` for kubelet scraping.
Network and disk observations can use C2's existing `pods/exec` permission to read `/proc/net/dev` and run `df` in the game pod.
Metrics Server supplies CPU and memory usage, not network throughput or disk usage.

## Run the disposable fixture test

```sh
bash scripts/kind-smoke.sh
```

The smoke test creates its own `rsdw-c2-smoke-<pid>` cluster through `kind-up.sh`, installs Metrics Server, and uses an explicit context.
It refuses to reuse an existing cluster with that name and deletes only its disposable cluster on exit.
The test reads fixture-pod metrics as C2's service account and node metrics as the fixture cluster administrator.
It checks C2's pod metrics read permissions and rejects node metrics, `nodes/proxy`, and `nodes/stats` access.
Pod assertions require a timestamp, a collection window, numeric CPU usage, and positive memory usage. A sleeping fixture can report zero CPU.
The test retains the metrics JSON and other logs under its printed temporary directory.
It does not start a game image. Docker access failures exit with status 2 and `INCONCLUSIVE`.

To run static checks without starting a cluster, use:

```sh
bash -n scripts/kind-up.sh scripts/kind-down.sh scripts/install-metrics-server.sh scripts/kind-smoke.sh
helm lint charts/rsdw-c2 --set auth.adminTokenSecret.name=rsdw-c2-admin
git diff --check
```

## Load a changed dashboard into an existing cluster

Keep the cluster stopped until you are ready to test. These commands resume it, install Metrics Server, and replace the dashboard image. They preserve Helm values, the admin Secret, and persistent world data.

```sh
bash scripts/kind-up.sh rsdw-c2-live
kind export kubeconfig --name rsdw-c2-live --kubeconfig /tmp/rsdw-c2-live.kubeconfig
docker build -t rsdw-c2:telemetry .
kind load docker-image rsdw-c2:telemetry --name rsdw-c2-live
helm upgrade rsdw-c2 charts/rsdw-c2 --namespace rsdw-system \
  --kubeconfig /tmp/rsdw-c2-live.kubeconfig --kube-context kind-rsdw-c2-live \
  --reuse-values --set image.repository=rsdw-c2 --set image.tag=telemetry \
  --set image.pullPolicy=Never --wait --timeout 120s
kubectl --kubeconfig /tmp/rsdw-c2-live.kubeconfig --context kind-rsdw-c2-live \
  -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 18083:8080
```

Open http://127.0.0.1:18083/. Allow two collection cycles for network rates and history. When finished, stop the port forward and run `bash scripts/kind-down.sh rsdw-c2-live`.
