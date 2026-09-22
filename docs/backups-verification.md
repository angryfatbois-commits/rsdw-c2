# Issue 37 verification

This records the implementation checks run on September 20, 2026. It distinguishes fixture evidence from validation against an actual game-generated backup.

## Requirement coverage

| Requirement | Implementation and verification |
| --- | --- |
| Dedicated local backup PVC, separate metadata, persistence disabled | Helm configuration, `backups_api_test.go`, `backup_limits_test.go`, chart checks in `scripts/verify.sh` |
| Writable Connected/Disconnected state and non-removable Local | Runtime storage checks, API tests, browser Storage page |
| Built-in profile protection, safe multi-item definitions | POST/PUT/import collision protection, revisions, capability/path validation; `backups_regression_test.go` and `backups_api_test.go` |
| Import/export and preview use one versioned schema | Strict single-document YAML, unknown/generated-field rejection, browser preview/import/export round trips |
| Real files and directories, including configuration and extensionless files | Filesystem and Kubernetes collector tests; real KIND capture of save plus `DedicatedServer.ini` |
| Running `.sav.backup`, never running `.sav`, stopped-state verification | Filesystem mutation/symlink tests, Kubernetes command tests, isolated KIND running/stopped captures and terminating-pod refusal |
| Always-bundled manifest and all item contents | One-item/multi-item ZIP tests, checksum checks, authenticated browser download |
| Limits preserve completed backups and remove failed staging | Independent repository/item/bundle limits and repeated quota rejection tests |
| Crash recovery and interrupted history | Recovery tests for staged and already-published artifacts, state reconciliation, interrupted runs and temporary-file cleanup |
| Schedules, timezones, no catch-up, extra Run now | API scheduling tests and browser daily/interval/cron editing, unavailable due occurrences, no advancement on manual Run now |
| Profile/schedule edits preserve intent | Revision checks, all daily times, interval units, omitted state rules, referenced-profile deletion refusal |
| Restore from Maintenance, tracked or Custom | Go-backed browser flows, actual filesystem transactions and isolated KIND byte checks |
| Separate destructive confirmation and stable existing target | Two-step browser confirmation, nonempty refusal, existing-PVC writes, config-only restore preservation |
| New server from backup stays stopped | Parsed fail-closed Helm post-rendering, upstream chart 0.1.1 render check, new PVC, KIND verification of zero replicas and restored bytes |
| Restore failure is retryable, never false success | Checksums before write; rollback journal retained until post-write stopped check; unsafe-name, unfinalized-recovery and state-change regression tests |
| Admin only, CSRF/auth patterns | Existing auth middleware; real OIDC viewer/anonymous route denial tests; viewer browser contracts |
| Discord backup events | Producer emits started/completed/failed with definition and source; alert and Discord message tests. No live Discord message sent. |
| Mockup-oriented UI | Operational history/schedules, separate Settings, compact searchable/scrollable profiles, form/action browser checks, no blue modal banners; screenshots linked below |

The tests do not establish pixel-identical rendering on every browser or exhaustive coverage of every possible cluster failure.

## Repeatable checks

```sh
bash scripts/verify.sh
go test -race ./...
```

The KIND test requires an explicitly selected local context. It creates a unique `rsdw-backup-test-*` namespace, runs its fixtures there, and deletes only that namespace afterward.

```sh
KUBECONFIG=/path/to/kind.kubeconfig \
RSDW_BACKUP_KIND_CONTEXT=kind-rsdw-c2-live \
go test -count=1 -run '^TestBackupKINDCaptureRestoreAndSafety$' -v .
```

Refresh the implementation screenshots after building:

```sh
go build -o .tmp/rsdw-c2 .
RSDW_BACKUP_SCREENSHOTS="$PWD/docs/screenshots/issue-37" node tests/backups-browser.cjs
```

See the [screenshot index](screenshots/issue-37/README.md).

## Corrected source convention

The real KIND game has `RSDragonwilds/Saved/SaveGames/TEST-01.sav` and `TEST-01.sav.backup`. The user confirmed that `.sav.backup` is the authoritative running-save convention; the earlier `.bak` requirement was incorrect. Collection now requires the full `.sav.backup` suffix, while stopped collection requires `.sav`. Restore removes only the trailing `.backup` to recover the `.sav` filename.

The persisted `running-bak` source tag is retained for compatibility with existing state and immutable bundles. It does not select a `.bak` file. Old tracked `.bak` bundles remain restorable; new collection does not fall back to `.bak`, a generic `.backup`, or a running `.sav`.

After this correction, the Go suite, race suite, vet, and both backup browser suites pass. The implementation screenshots and design references were refreshed. The updated KIND test could not run: the earlier kubeconfig and Docker socket are absent in this session. Earlier KIND results in the coverage table prove the capture/restore mechanics with the previous synthetic `.bak` fixture, not a fresh cluster run of the corrected suffix.
