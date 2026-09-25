# Backups

RSDW C2 stores immutable backup bundles separately from each game server's world PVC and from `state.json`. Two storage backends are available: a local filesystem backend mounted at `/var/lib/rsdw-c2/backups` on a dedicated C2 persistent volume, and an S3-compatible remote backend for off-VPS durability. The Helm chart exposes local storage as `backups.persistence`; disabling C2 persistence also disables the local backend. Local remains the default and cannot be removed; adding an S3 backend is optional and additive.

## S3-compatible remote storage

Connect an S3-compatible bucket (Backblaze B2, Wasabi, DigitalOcean Spaces, MinIO, AWS S3, or any S3-compatible API) as an additional backup destination from the Backups → Storage settings page. C2 never receives or stores raw credentials in `state.json`; it only stores the name of a Kubernetes Secret you create yourself, matching how Discord webhook tokens are handled.

**Before connecting a backend**, create a Secret in the `rsdw-system` namespace (or your configured `RSDW_NAMESPACE`) with two fixed keys:

```sh
kubectl create secret generic rsdw-s3-offsite \
  --namespace rsdw-system \
  --from-literal=accessKeyId="$S3_ACCESS_KEY_ID" \
  --from-literal=secretAccessKey="$S3_SECRET_ACCESS_KEY"
```

Then, in the dashboard, click **Add storage** and provide:

| Field | Meaning |
| --- | --- |
| Display name | Shown on the storage card; also used to derive the backend's internal id |
| Endpoint | Host and optional port, no scheme (for example `s3.us-west-002.backblazeb2.com`) |
| Bucket | The bucket name; must already exist |
| Region | Optional; some S3-compatible services ignore it |
| Path prefix | Optional key prefix inside the bucket (for example `rsdw-backups/`) |
| Use SSL | Enabled by default |
| Secret name | The name of the Secret created above |

C2 test-connects the bucket (bucket existence and a write/delete probe) before persisting the backend record, so a misconfigured endpoint, bucket, or Secret surfaces immediately as a form error rather than at the next scheduled run.

Once connected, the backend appears as a destination option on the backup run form and on schedule forms, alongside Local. Existing runs and schedules keep referencing whichever backend they were created against; nothing migrates automatically between backends.

**Storage model.** Bundle assembly (zipping the manifest and captured items) always happens on local disk first, exactly as it does for the Local backend; only the finished `bundle.zip` and its `publication.json` record are uploaded, as two objects, to `<prefix>objects/<manifestID>/`. There is no multipart upload path: uploads are single `PutObject` calls, which comfortably covers the default 256 MiB bundle limit. The same `RSDW_BACKUP_MAX_REPOSITORY_BYTES`, `RSDW_BACKUP_MAX_ITEM_BYTES`, and `RSDW_BACKUP_MAX_BUNDLE_BYTES` limits documented below apply per S3 backend as well as to Local.

**Crash recovery.** Local storage reconciles interrupted captures and staged-but-unpublished bundles on every C2 restart. S3 does not: an interrupted upload can leave an orphan `bundle.zip` without a matching `publication.json`, or vice versa. C2's recovery pass silently skips incomplete pairs on an S3 backend rather than failing the whole recovery; a retried backup run with the same idempotency key detects and safely completes over the orphaned pair. This is a deliberate v1 tradeoff to avoid adding S3-specific locking; it does not affect data integrity of already-completed bundles.

**Removing a backend.** Removing an S3 backend from the dashboard stops C2 from publishing new backups to it; already-published bundles remain in the bucket untouched. Removal is rejected while any enabled schedule still targets that backend — disable or reassign the schedule first. The Local backend cannot be removed.

**Reconnection after restart.** On every C2 restart, each persisted S3 backend's Secret is re-resolved and its connectivity re-verified. If the Secret has been deleted or rotated incompatibly, the backend is marked disconnected in the dashboard rather than crashing C2 startup; reconnect by re-adding the backend once the Secret is restored.

A Kubernetes PVC backed by NFS can also use the local filesystem backend directly without needing an S3-specific setup, if network file shares are preferred over an S3-compatible API.

## Profiles and manifests

A backup profile is a versioned `BackupDefinition`. It declares one or more logical items, such as a world save and a server configuration file. Each item has a safe, server-type-relative source rule for running and stopped states. Paths are not entered on each run and are never arbitrary filesystem paths.

The profile can be authored in the Backups settings page or imported and exported as YAML. A completed run creates a separate generated manifest containing the resolved source path, item kind, size, checksum, consistency result, and opaque stored object key. The binary items and manifest are published atomically as one bundle. Downloads always produce a bundle, including one-item backups.

The built-in Dragonwilds profile declares the relative save directory:

```text
RSDragonwilds/Saved/SaveGames
```

C2 resolves the expected source inside that declared directory at run time. The running source is a server-generated `.sav.backup`; the stopped source is the flat `.sav`. The exact filename is not assumed. A run requires the expected extension and an unambiguous source. A missing, unstable, transitioning, or ambiguous source fails without publishing a partial backup.

## Source safety

- Running servers use only the `.sav.backup` source. C2 never falls back to `.sav` while a server is running.
- Stopped servers use only the `.sav` source after C2 verifies the server is actually stopped.
- Running files are checked for stable size and modification time during capture. The game should publish `.sav.backup` atomically.
- Directory items are captured through the profile collector with traversal and symlink escape checks.
- Repository, per-item, and bundle limits are configurable. When a limit would be exceeded, the new backup fails, its rejected staging data is removed, and existing bundles are retained.

## Storage limits and health

The defaults are 9 GiB of published bundles per repository, 32 MiB per item, and 256 MiB per bundle. The bundle limit also bounds the combined item contents before compression. Directory items count as their captured tar payload, including tar headers. The repository quota counts published ZIP bundle bytes, not total filesystem usage.

| Limit | Environment variable | Helm value | Default bytes |
| --- | --- | --- | --- |
| Repository | `RSDW_BACKUP_MAX_REPOSITORY_BYTES` | `backups.maxRepositoryBytes` | 9663676416 |
| Item | `RSDW_BACKUP_MAX_ITEM_BYTES` | `backups.maxItemBytes` | 33554432 |
| Bundle | `RSDW_BACKUP_MAX_BUNDLE_BYTES` | `backups.maxBundleBytes` | 268435456 |

Overrides must be positive integers. Invalid environment values fail controller initialization. The chart's default 10 GiB backup PVC leaves 1 GiB beyond the published-bundle quota for capture, staging, and metadata. This headroom is not a reservation or a guarantee against a full volume. Keep it when adjusting limits or PVC capacity.

Each backup listing checks current storage health once and uses that result for availability, backend status, and usage. The local check writes and removes a probe in the repository root, staging directory, and objects directory, then validates publication metadata and uses file stats to confirm that each bundle is a regular file of the recorded size. It totals bundle sizes without reading or hashing bundle contents. Missing or truncated bundles, invalid publication metadata, and unwritable storage report unavailable and disconnected, with `limits.usedBytes` set to `null` and a `storageError` message. A healthy response includes numeric usage, an empty `storageError`, and `limits.maxBundleBytes` alongside the repository and item limits.

Usage checks do not detect corruption that preserves a bundle's size. Full checksum validation remains part of startup recovery and actual restore, so a connected status indicates storage availability, not verified content integrity.

## Retention and pruning

The quota limits above are purely rejective: when a limit would be exceeded, the new backup fails and existing bundles are retained (see "Source safety"). Retention is the first proactive deletion of successful, already-published bundles, and applies only to schedules, not manual runs.

Each schedule optionally sets `retainCount` (keep the newest N successful runs) and/or `retainDays` (keep runs newer than N days). A run is kept if it satisfies either condition; it is pruned only once both configured conditions exclude it. Leaving both at 0 (the default) disables pruning for that schedule — old runs accumulate until the repository quota is hit. Manual runs, and runs from other schedules, are never touched by a schedule's retention settings.

Pruning runs synchronously after each successful scheduled backup, outside the run's own lock. C2 deletes the pruned run's bundle and publication record from the backend that produced it, then removes the run and its manifest from state. Deletion applies identically to Local and S3-compatible backends. If the backend delete call fails, the run stays in state and is retried on the next successful scheduled run.

A completed run can also be deleted manually with `DELETE /api/backups/runs/{id}`, which performs the same backend cleanup. C2 refuses to delete a run that is still in progress.

## Restart recovery

Startup recovery removes interrupted `.capture` and `.restore-*` temporary data, completes valid staged publications, and discovers already published bundles. Staged publications that exceed the configured quota are removed. Recovery reconciles manifests and runs with durable publications, preserving existing run identity, schedule association, and creation time. It records successful publications even if the earlier state update failed, creates a run for an orphan publication, and marks unfinished runs without a publication as interrupted. Repeated recovery does not duplicate runs. Published data that fails validation causes recovery to return an error.

## UI

Backups is an admin-only tab immediately below Reboots. The overview shows health, recent runs, source state, bundle size, item count, storage status, and upcoming schedules. The settings cog opens compact, searchable, scrollable profile management and storage backend cards. The local backend is connected or disconnected and cannot be removed.

Schedules use the same timezone behavior as Reboots. A manual Run now on a schedule creates an extra run and leaves the next scheduled occurrence unchanged. Schedules skip overdue occurrences after downtime instead of creating a catch-up burst.

If storage is disabled or disconnected when a schedule becomes due, the scheduler records `LastRun`, a failed `LastResult`, and `LastError`, then advances `NextRun` beyond the current time. Reconnecting storage does not replay that failed occurrence. If the schedule cannot be recalculated, it is disabled and its next run is cleared.

Backup details intentionally offer only Download bundle. Restore lives in server Maintenance and supports a tracked backup or a custom `.sav` upload. C2 verifies the stopped target and checks for existing save data. A nonempty target requires a separate destructive confirmation. Creating a new server from a backup is also available from the restore flow. Viewer accounts cannot list, download, or restore backups.

Restore validates bundle and item checksums, stages incoming files, and journals replacements on the target PVC. Original files remain available until a post-write check confirms the server is still stopped. If that check fails, the operation reports failure and retains recovery data for the next stopped restore attempt. Config-only restores preserve existing world saves. New-server restores use a fail-closed Helm post-renderer to provision the workload with zero replicas before writing its new PVC.

## Production path investigation

The upstream Dragonwilds chart and earlier local KIND inspection confirmed the server data mount as `/home/steam/rsdw-dedicated` and the save directory above. Earlier isolated KIND runs verified capture and restore mechanics using a synthetic `.bak` fixture. The integration fixture now uses `World.sav.backup` to match the confirmed convention. Its rerun is currently blocked because the session no longer has the KIND kubeconfig or Docker socket; Go filesystem tests and browser tests pass with the corrected suffix.

The existing read-only KIND Dragonwilds test server contains `TEST-01.sav` and `TEST-01.sav.backup` under that directory. The user confirmed `.sav.backup` as the required running-save convention, correcting the earlier `.bak` assumption. The runtime collector discovers that full suffix, requires exactly one match, and rejects ambiguous results. It never falls back to a running `.sav`, `.bak`, or generic `.backup` file. Restoring `TEST-01.sav.backup` produces `TEST-01.sav`.

Screenshots of the implemented flows are in [`docs/screenshots/issue-37`](screenshots/issue-37).

See [verification and source-convention compatibility](backups-verification.md) for repeatable commands and details of the retained legacy `running-bak` metadata tag.
