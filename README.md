# RSDW C2

<p align="center">
  <img src="docs/readme-banner.png" alt="RSDW c2 dashboard, telemetry, and server setup screens" width="100%">
</p>

<p align="center">
  <a href="https://github.com/angryfatbois-commits/rsdw-c2/actions/workflows/ci.yml"><img src="https://github.com/angryfatbois-commits/rsdw-c2/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/angryfatbois-commits/rsdw-c2/releases"><img src="https://img.shields.io/github/v/release/angryfatbois-commits/rsdw-c2?display_name=tag" alt="Latest release"></a>
  <a href="https://github.com/angryfatbois-commits/rsdw-c2/blob/main/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/angryfatbois-commits/rsdw-c2" alt="Go version"></a>
  <a href="https://github.com/angryfatbois-commits/rsdw-c2/issues"><img src="https://img.shields.io/github/issues/angryfatbois-commits/rsdw-c2" alt="Open issues"></a>
</p>

A small web control panel for RuneScape: Dragonwilds dedicated servers on Kubernetes.

I built this for my own server and a few friends. It keeps the useful stuff in one place so we can check a server, read its logs, restart it, or roll out a new image without digging through Kubernetes by hand.

This is a personal project, not a hosted service. Expect opinionated defaults and a few rough edges.

## What it does

- Shows server health, players, uptime, events, logs, and telemetry.
- Creates Dragonwilds servers from the `rsdragonwilds-helm` chart.
- Restarts servers and updates their container image.
- Schedules per-server reboots with cron, elapsed intervals, daily local times, and IANA timezones. See [scheduled reboots](docs/reboots.md) for the single-replica and persistent-state requirements.
- Creates server-aware backup bundles from verified `.sav.backup` or stopped `.sav` sources. See [backups](docs/backups.md) for storage, profiles, schedules, and restore behavior.
- Deletes servers while keeping their world by default. See [server deletion](docs/server-deletion.md) for the details.
- Imports optional `.sav` files and saves named player IDs.
- Sends Discord alerts and supports OIDC sign in with admin and viewer roles.

## Try it locally

You need Go 1.26 or newer.

Clone this repository, then run these commands from its root:

```sh
RSDW_DEMO_DATA=true RSDW_STATE_FILE=./state.json go run .
```

Open [http://localhost:8080](http://localhost:8080). Demo mode gives you sample data without a Kubernetes cluster.

## Install it on Kubernetes

Use the published Helm chart for a real cluster. You need `kubectl`, Helm, and OpenSSL.

```sh
VERSION=1.2.0
export RSDW_C2_TOKEN="$(openssl rand -hex 32)"

kubectl create namespace rsdw-system
kubectl -n rsdw-system create secret generic rsdw-c2-admin \
  --from-literal=token="$RSDW_C2_TOKEN"

helm upgrade --install rsdw-c2 oci://ghcr.io/angryfatbois-commits/charts/rsdw-c2 \
  --version "$VERSION" \
  --namespace rsdw-system \
  --set auth.adminTokenSecret.name=rsdw-c2-admin

kubectl -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 8080:8080
```

Open [http://localhost:8080](http://localhost:8080) and sign in with `$RSDW_C2_TOKEN`.

Replace `VERSION` with a version from the [GitHub releases](https://github.com/angryfatbois-commits/rsdw-c2/releases). Use the [deployment notes](docs/oidc.md) for OIDC, ingress, and other cluster setup.

Scheduled reboots require the chart's default single replica and persistent state volume. Do not enable them with multiple C2 replicas or ephemeral state; the scheduler intentionally has no leader-election layer.

## Docs

- [Configure OIDC sign in](docs/oidc.md)
- [Configure Discord alerts](docs/integrations.md)
- [Understand telemetry](docs/telemetry.md)
- [Import custom saves](docs/custom-saves.md)
- [Delete servers safely](docs/server-deletion.md)
- [Schedule server reboots](docs/reboots.md)
- [Back up and restore worlds](docs/backups.md)
- [Run a local kind cluster](docs/kind.md)
- [Read the architecture notes](ARCHITECTURE.md)

## Check the project

```sh
npm ci --ignore-scripts
npx --no-install playwright install chromium
bash scripts/verify.sh
```

If something breaks, [open an issue](https://github.com/angryfatbois-commits/rsdw-c2/issues). I built this for a small group, but bug reports are still welcome.
