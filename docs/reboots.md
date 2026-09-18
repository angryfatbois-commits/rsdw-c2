# Scheduled reboots

RSDW C2 can schedule restarts for individual servers. The **Reboots** page is admin-only and uses the same restart operation, telemetry reconciliation, JSON state file, and Discord event pipeline as a manual restart. The scheduler is one bounded polling loop inside C2; it does not create Kubernetes CronJobs, use a separate job service, or start one goroutine per schedule.

Run this feature with one C2 replica and persistent state. The chart already constrains `replicaCount` to one, uses a `Recreate` deployment strategy, and enables the state PVC by default. The single-writer contract is an operational requirement, not leader election. An ephemeral state volume is suitable for demo or disposable verification only. Set `RSDW_REBOOTS_ENABLED=false` to stop new enabled schedules from executing or being saved; administrators can still inspect, disable, and delete existing schedules.

## Timing modes

Every schedule stores an IANA execution timezone. The browser sends intent only; C2 owns the schedule ID, UTC anchor, next run, occurrence record, and restart operation ID.

- **Cron** uses exactly five standard fields: minute, hour, day of month, month, and day of week. Lists, ranges, steps, month names, and weekday names are accepted by the pinned `robfig/cron` parser. Seconds, `@` descriptors, `TZ=` or `CRON_TZ=` prefixes, and Quartz `?`, `L`, `W`, and `#` syntax are rejected. Sunday is `0` or `SUN`. When both day-of-month and day-of-week are restricted, standard cron OR semantics apply.
- **Interval** accepts 1–8,760 hours or 1–365 days. A day means exactly 24 elapsed hours, including across daylight-saving transitions. C2 persists a UTC anchor and calculates `anchor + N * duration`, so execution and failure do not cause drift.
- **Daily** accepts one or more unique `HH:mm` values. Values are sorted and deduplicated and interpreted as local wall-clock minutes in the stored execution timezone.

Creation, re-enabling, or changing the target, timing, or execution timezone schedules the first run strictly after the save. Saving never restarts a server immediately. Disabling clears the next run while retaining history. Changing the display timezone does not change an existing execution timezone.

C2 resolves cron and daily wall-clock minutes through the same calculator used by preview and execution. A nonexistent spring-forward minute is skipped. A repeated fall-back minute is recorded once at its first UTC occurrence; a cursor between the two copies does not select the second copy. Intervals do not use this wall-clock resolver.

The **Preview next five runs** action returns five UTC instants and the execution timezone. The browser only formats those instants; it never calculates a second schedule. The display-timezone preference is browser-local and scoped to the authenticated subject. It defaults to the browser's IANA zone. Token mode has the one shared `token-admin` identity by design.

## Execution and recovery

The scheduler scans approximately every 15 seconds. A due occurrence may be up to 60 seconds late. Older unclaimed occurrences are summarized as missed and advanced directly to the next future run; C2 does not emit catch-up bursts. Startup recovery skips all occurrences due during downtime, even when they are less than 60 seconds old. Interval anchors are never moved by recovery.

For each server, C2 rereads the current schedule, target, enabled state, next run, and pending restart inside one `Store.Update`. It advances the occurrence and persists its audit record before calling the orchestrator. Two due schedules for one server share one restart operation. If a manual or scheduled restart is already pending, a new due occurrence is recorded as skipped rather than attached to that operation. Missing targets and invalid schedules make zero external calls. Occurrences for a stopped server are skipped and advanced with the reason `Target server is stopped`; the schedule itself may remain. A deleting target still cannot receive new schedules.

The store commit must succeed before any Kubernetes command is attempted. External orchestration happens outside the store mutex. A command failure is uncertain and is never retried automatically. A successful command is still only awaiting reconciliation until telemetry observes a fresh healthy runtime carrying the restart annotation. The existing five-minute readiness fence records failure only after a fresh definitive observation; unknown telemetry does not become a fake completion or failure. C2 never redispatches an uncertain operation after restart.

Disabling, editing, or deleting a schedule before its claim prevents that old occurrence. A committed claim is irrevocable: it can finish after the schedule is edited or deleted. Deleted schedules retain their occurrence history so an in-flight operation remains auditable. Server deletion uses the same lifecycle lock as restart claims, so a deletion cannot race a scheduled claim; once deletion has a receipt, future work for that server is not dispatched.

History distinguishes `awaiting_reconciliation`, `uncertain`, `completed`, `failed`, `skipped`, and `missed`. Terminal history is bounded to 500 records while active operation links are retained. Schedule mutations include the authenticated actor in the C2 event log. Scheduled restart events use the existing restart alert kinds, so Discord integrations do not receive a second completion notification.

Scheduled reboots proceed even when players are connected. The form always shows the disconnect warning, and enabling a schedule requires an explicit acknowledgment. An unavailable player count is shown as unavailable; it is never interpreted as zero.

## Memory pressure restarts

Memory pressure restarts are disabled by default. The policy applies to every server and is configured at C2 startup, without a policy API or UI.

| Environment variable | Helm value | Default |
| --- | --- | --- |
| `RSDW_MEMORY_PRESSURE_ENABLED` | `memoryPressure.enabled` | `false` |
| `RSDW_MEMORY_PRESSURE_THRESHOLD_PERCENT` | `memoryPressure.thresholdPercent` | `85` |
| `RSDW_MEMORY_PRESSURE_DURATION` | `memoryPressure.duration` | `10m` |

The enabled value must be a boolean, the threshold must be between 0 and 100 inclusive, and the duration must be a positive Go duration such as `10m` or `1m30s`. Helm values accept whole-unit components up to six digits and reject values outside the runtime duration range. Invalid values prevent startup, even when the policy is disabled.

The existing collector checks fresh container memory usage against its observed memory limit. Usage at or above the threshold must persist for the configured duration on the same runtime. The streak uses collection completion timestamps. Missing or invalid readings, stale observations, gaps longer than 45 seconds, runtime changes, stopped or deleting servers, and pending restarts reset the streak. C2 restart also clears the streak, so downtime never counts toward a restart.

A pressure claim persists before dispatch and uses the existing restart reconciliation. It emits `memory_pressure_restart_requested` with warning severity, followed by the shared `restart_completed` or `restart_failed` event. Manual and scheduled requests retain `restart_requested`. Enable the pressure rule separately in a Discord integration to receive those requests. Restarts can disconnect active players.

Demo mode checks synthetic observations from the seeded server's memory fields and simulates completion. It never calls Kubernetes or Discord for this flow.

## API

All routes require an admin principal. Anonymous requests receive `401`, viewers receive `403` before body parsing or schedule calculation, and OIDC writes—including preview—use the existing Origin and CSRF checks.

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/reboots` | List schedules, execution history, demo state, and availability |
| POST | `/api/reboots` | Create a schedule; returns `201` |
| PUT | `/api/reboots/{id}` | Replace a schedule; returns `200` |
| DELETE | `/api/reboots/{id}` | Remove future work; returns `204` |
| POST | `/api/reboots/preview` | Validate intent and return five future UTC instants without writing state |

Create and update use this flat body. Only fields for the selected mode are accepted; contradictory fields are rejected.

```json
{
  "serverId": "scuffedtards",
  "enabled": true,
  "mode": "daily",
  "dailyTimes": ["05:00", "17:00"],
  "executionTimezone": "America/New_York",
  "acknowledgeDisconnect": true
}
```

Use `mode: "cron"` with `cron`, or `mode: "interval"` with `intervalValue` and `intervalUnit`. Preview does not require a target server, but it validates a supplied target when present. Preview has no state, event, claim, or Kubernetes side effect.

Invalid JSON, unknown fields, unsupported zones, malformed timing, missing targets, and missing enabled-save acknowledgment return `400` with an `error` string and, when applicable, a `fields` object for inline form errors. Persistence failures return `500`. The UI retains the draft after a failed request and focuses its error summary.

## Verification

The Go tests cover literal UTC expectations for all modes, strict-future behavior, interval anchors, cron validation, preview parity, New York spring gaps and fall folds, same-server coalescing, missing targets, restart reconciliation, and no dispatch after a persistence failure. Run:

```sh
go test ./...
go test -race ./...
node --test web/app.test.cjs
bash scripts/verify.sh
```

The disposable kind checks remain focused on chart installation and restart behavior. They do not claim durable scheduling guarantees unless they mount persistent state and keep the one-replica contract.
