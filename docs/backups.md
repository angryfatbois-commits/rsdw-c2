# Backups

RSDW C2 stores immutable backup bundles separately from each game server's world PVC and from `state.json`. The v1 backend is a local filesystem mounted at `/var/lib/rsdw-c2/backups` on a dedicated C2 persistent volume. The Helm chart exposes this as `backups.persistence`; disabling C2 persistence also disables backups.

S3-compatible services and network file shares are future storage backends. A Kubernetes PVC backed by NFS can use the current filesystem backend without adding an S3-specific implementation.

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
