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
  category, severity, message, details,
  actor, scheduleId, occurrenceId, operationId
}

RebootSchedule {
  id, definition, enabled, revision, createdAt, updatedAt,
  intervalAnchor, nextRun, lastOccurrenceAt, lastResult, lastReason
}

RebootExecution {
  id, occurrenceId, scheduleId, revision, serverId, occurrenceAt, recordedAt,
  operationId, result, reason
}

ServerSpec {
  name, namespace, region, ownerId, imageTag, maxPlayers
}
```

`Server.status` is a small state set. It is `online`, `starting`, `attention`, `stopped`, or `unknown`. `updateAvailable` is true when the desired image differs from the image recorded as current. The API validates external JSON into these shapes before the service uses it.

## Runtime boundaries

`StateStore` reads and writes the JSON document. It serializes writes behind one mutex and writes a temporary file before renaming it into place. A post-rename directory-sync failure poisons the store until restart so a later mutation cannot overwrite a claim whose durable status is uncertain.

`Orchestrator` owns Helm and `kubectl` execution. The demo implementation updates the local state without starting a cluster process. The Kubernetes implementation creates the namespace and API token Secret, then runs `helm upgrade --install` with the chart values required by `rsdragonwilds-helm`. Refresh reads Deployment readiness and image state, then queries the game container's authenticated `/api/health` and `/api/players` endpoints for live readiness, uptime, and player count. Resource metrics remain unavailable until a Kubernetes metrics source is connected.

The HTTP handlers parse requests, call a domain operation, and encode JSON or the embedded web files. They do not contain Helm argument rules or UI-specific state.

## API contract

`Auth` owns authentication, OIDC transactions, and opaque sessions. A typed principal has a denied zero value, viewer, or admin role. Token mode grants the admin role for a matching bearer token. OIDC mode requires an exact subject or group assignment from a verified ID token and rejects bearer credentials. Demo data does not bypass OIDC.

The HTTP boundary authorizes requests before handlers parse bodies or call the orchestrator. Viewers may only GET bootstrap and a server's telemetry. Unknown routes are denied for viewers. Dedicated viewer response types select display metadata and numeric metrics; they omit operational data and replace collector errors with fixed availability messages. Admin operations include check-update because it changes stored state.

OIDC login uses authorization code flow, S256 PKCE, browser-bound single-use state, and nonce verification. Sessions expire absolutely and remain in bounded process memory. Logout revokes the session. Cookie-authenticated writes require an exact public Origin and a session CSRF token. The UI uses server capabilities, clears protected state when identity changes, and rejects late responses from older sessions. See [OIDC setup and session behavior](docs/oidc.md).

```text
GET  /api/auth
POST /api/session                 (token mode)
GET  /api/auth/login              (OIDC mode)
GET  /api/auth/callback           (OIDC mode)
POST /api/auth/logout             (OIDC mode)
GET  /api/bootstrap
GET  /api/servers/:id/logs?tail=100
GET  /api/servers/:id/telemetry?range=60s
POST /api/servers
POST /api/servers/:id/actions/restart
POST /api/servers/:id/actions/update
POST /api/servers/:id/actions/check-update
GET  /api/events?query=&category=&serverId=&since=&limit=&offset=
GET  /api/reboots
POST /api/reboots
PUT  /api/reboots/:id
DELETE /api/reboots/:id
POST /api/reboots/preview
```

The service emits an `Event` for every mutating action. Restart and update require an explicit confirmation in the browser before the browser sends the request. Scheduled reboots use one polling loop and atomically persist the due occurrence, next-run cursor, linked restart operation, and audit event before dispatching the existing orchestrator command. Preview and execution share one cron/daily civil-time calculator; nonexistent wall-clock minutes are skipped and repeated minutes use their first UTC occurrence. See [scheduled reboots](docs/reboots.md) for the timing, recovery, and single-replica contract.

## First-draft scope

The console includes Dashboard, Telemetry, Events, Maintenance, Saved IDs, Integrations, and Reboots. Every visible button, selector, filter, chip, and modal maps to a working local state change or an API call. Export buttons download the current filtered records as CSV. Reboot execution is intentionally best effort across process downtime: startup skips overdue occurrences, uncertain external commands are not retried, and a claimed operation cannot be canceled by editing or deleting its schedule.

The first update check refreshes deployment/game state and makes a best-effort anonymous OCI Registry tag-list request for semver tags. It marks a server when the newest visible tag differs from the running image. Private registries can still rely on the desired-image drift check until registry credentials are added.

## Verification

`scripts/verify.sh` checks Go tests, the built binary, API behavior, UI route content, and rendered Helm manifests. The browser pass exercises the same controls a user sees. A kind pass deploys the control-plane chart with a low resource budget and checks its Service and Pod. The game chart uses `helm template` in the local pass because its upstream image download is not suitable for a constrained laptop smoke test.
