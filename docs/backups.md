# World backups

RSDW C2 can copy a Dragonwilds world save out of a server's volume, keep it as a downloadable bundle, and write one back. The **Backups** page is admin-only. Viewers never see the page, the nav item, or any backup route; every `/api/backups` request and the `restore` server action answer `403` for them.

Backups require persistent C2 state. The bundle is only useful with the run record that describes it, so when C2 runs on an ephemeral state volume the feature reports itself unavailable instead of writing files nobody can find again. Set `RSDW_BACKUPS_ENABLED=false` to turn it off explicitly. When backups are unavailable, existing history stays visible and every backup route answers `503` with `backups require persistent C2 state storage`.

Like reboots, this is one bounded polling loop inside a single C2 replica. It creates no Kubernetes CronJobs and no job service.

## What gets collected

v1 ships exactly one built-in profile, `dragonwilds-world-save`, using the `logical-files` strategy. It is read-only; there is no profile authoring UI and no YAML import.

C2 decides the source from verified server state, never from the status text on screen:

- A **running** server contributes only the game-generated `.bak` in `RSDragonwilds/Saved/SaveGames`. C2 never reads the live `.sav` from a running world, because the game rewrites that file in place and a copy taken mid-write is silently corrupt.
- A **stopped** server contributes only the flat `.sav` in the same directory. A stopped world is a perfectly valid backup source, so stopping a server does not disable its schedule.
- Anything else — starting, no active Pod, or an unreadable deployment — is skipped with `Server is in a transitional state; backups require a running or stopped server`.

There is **no fallback between the two**. A running server with no `.bak` fails rather than reaching for the `.sav`.

The save's filename is discovered at runtime, never assumed. Different worlds use different names. If the directory holds no candidate, or more than one, the run fails and names what it found; C2 does not guess which world to back up.

Because the game does not publish `.bak` atomically, C2 lists the directory twice about two seconds apart and requires identical size and modification time before copying. A file that moved under it fails the run with `Backup source changed while it was being read`.

A stopped server has no container to exec into. C2 creates a short-lived `rsdw-backup-<id>` Pod that mounts the world volume read-only, copies the save out, and deletes the Pod — including on every failure path, and including when the request that started the run was cancelled.

## Where bundles are stored

Bundles live on a dedicated backup volume, never on the state volume. A full backup volume must not be able to break state writes.

The chart provisions this by default:

```yaml
backups:
  enabled: true
  size: 20Gi
  storageClass: null
  maxRepositoryBytes: 0
```

`maxRepositoryBytes: 0` derives the limit from the volume; set a non-zero byte count to cap it lower. The claim is `<release>-backups`, mounted at `/var/lib/rsdw-c2/backups`.

A bundle is an uncompressed `tar` containing `manifest.json` first, then the collected save under `items/`. The payload is already-compressed game data, so a compression pass would cost CPU for near-zero gain. Downloads are served as `application/x-tar`. The manifest records the run and server identity, the source mode, the original path, the byte size, and a `sha256:` checksum of the collected file.

Publishing is atomic. Bytes are written to a temporary name, flushed, then renamed; a run is only recorded `completed` after that rename succeeds. A bundle interrupted by a process death is never listed as downloadable, and startup recovery removes the leftover working files and marks any run still labelled `running` as failed with `C2 restarted while this backup was running`.

**A full repository fails new backups. C2 never deletes an existing bundle to make room.** The run fails with `Backup repository limit reached; existing backups were preserved`. Delete a backup yourself to reclaim space. There is no automatic retention or rotation in this version; bundles go away only when you remove them with `DELETE /api/backups/runs/<id>` or the Delete button in history.

## Schedules

Backup schedules reuse the reboot scheduler's timing rules exactly: cron, elapsed interval, or daily wall-clock times, each with a stored IANA execution timezone, and the same daylight-saving resolution. See [scheduled reboots](reboots.md) for the timing semantics in detail.

Warning minutes do not apply to backups and a non-zero value is rejected. Backups do not disconnect players, so there is nothing to warn about.

The scheduler scans about every 15 seconds and tolerates an occurrence up to 60 seconds late. Anything later is recorded as missed and the cursor jumps to the next strictly-future occurrence; C2 never emits catch-up bursts. Startup recovery skips occurrences that were due while C2 was down.

One server runs one backup at a time. A second due occurrence for a busy server is recorded as skipped rather than queued. The schedule cursor advances inside the same state write that claims the run, before any Kubernetes call.

**Run now** is separate from the schedule. Both the page-level *Run backup now* and the per-schedule *Run now* create an extra run and leave `nextRun` untouched.

## Restoring a world

Restore lives on the server's **Maintenance** panel, not on the Backups page, and only appears for a stopped server.

- **The target server must be stopped.** Writing a save underneath a live game process corrupts it, so a restore against a running server is refused with `409`.
- **A non-empty world is protected.** If the target already holds save data, the restore is refused with `target world already contains save data; confirm replacement explicitly` unless you type `REPLACE WORLD <serverId>` into the confirmation field.

The source is either a tracked backup from history or an uploaded `.sav`. Uploads use the same limits as save import: one `.sav` file of at most 32 MiB with a plain filename, no path separators, and no control characters.

C2 writes through a short-lived `rsdw-restore-<id>` Pod that mounts the world volume writable, copies the file to a temporary name, then `chmod`s, syncs, and renames it into place. The Pod is always deleted afterwards. A restored `.bak` is published as the live `.sav`. The outcome is recorded as a system event on that server.

Start the server when you are ready; restore never starts it for you.

## Alerts

The `backup_started`, `backup_completed`, and `backup_failed` alert rules are available and can be enabled on any Discord or webhook integration. See [integrations](integrations.md).

## Demo mode

Demo mode synthesizes a completed backup with a small fixed manifest. It performs no Kubernetes operation and reads no world volume.
