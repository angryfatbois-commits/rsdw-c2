# RSDW C2 architecture

RSDW C2 is a small Go service with a static web console. The service owns the managed-server records and translates user actions into Helm and Kubernetes commands. The browser never talks to Kubernetes directly.

## Data model

The service stores one JSON document with these records:

```text
Server {
  id, name, namespace, release, region,
  ownerId, currentImage, desiredImage,
  status, players, maxPlayers, tickRate,
  cpuPercent, memoryUsedBytes, memoryLimitBytes, diskPercent,
  networkBytesPerSecond, uptimeSeconds, lastRestart, lastSeen,
  updateAvailable, endpoint
}

Event {
  id, timestamp, serverId, serverName,
  category, severity, message, details
}

ServerSpec {
  name, namespace, region, ownerId, imageTag, maxPlayers
}
```

`Server.status` is a small state set. It is `online`, `starting`, `attention`, `stopped`, or `unknown`. `updateAvailable` is true when the desired image differs from the image recorded as current. The API validates external JSON into these shapes before the service uses it.

## Runtime boundaries

`StateStore` reads and writes the JSON document. It serializes writes behind one mutex and writes a temporary file before renaming it into place.

`Orchestrator` owns Helm and `kubectl` execution. The demo implementation updates the local state without starting a cluster process. The Kubernetes implementation creates the namespace and API token Secret, then runs `helm upgrade --install` with the chart values required by `rsdragonwilds-helm`. Refresh reads Deployment readiness and image state, then queries the game container's authenticated `/api/health` and `/api/players` endpoints for live readiness, uptime, and player count. Resource metrics remain unavailable until a Kubernetes metrics source is connected.

The HTTP handlers parse requests, call a domain operation, and encode JSON or the embedded web files. They do not contain Helm argument rules or UI-specific state.

## API contract

```text
GET  /api/bootstrap
GET  /api/servers/:id/logs?tail=100
GET  /api/servers/:id/telemetry?range=60s
POST /api/servers
POST /api/servers/:id/actions/restart
POST /api/servers/:id/actions/update
POST /api/servers/:id/actions/check-update
GET  /api/events
```

The service emits an `Event` for every mutating action. Restart and update require an explicit confirmation in the browser before the browser sends the request. The server checks the target ID and request body at the HTTP boundary.

## First-draft scope

The console includes Dashboard, Telemetry, Events, and Maintenance. It leaves backups and integrations out of the navigation. Every visible button, selector, filter, chip, and modal maps to a working local state change or an API call. Export buttons download the current filtered records as CSV.

The first update check refreshes deployment/game state and makes a best-effort anonymous OCI Registry tag-list request for semver tags. It marks a server when the newest visible tag differs from the running image. Private registries can still rely on the desired-image drift check until registry credentials are added.

## Verification

`scripts/verify.sh` checks Go tests, the built binary, API behavior, UI route content, and rendered Helm manifests. The browser pass exercises the same controls a user sees. A kind pass deploys the control-plane chart with a low resource budget and checks its Service and Pod. The game chart uses `helm template` in the local pass because its upstream image download is not suitable for a constrained laptop smoke test.
