# Issue 37 backup UI mockups

These mockups use the current Dragonwilds Control shell and `web/styles.css` tokens. They show the proposed Backups page directly below Reboots, with operational backup data separated from Backup Settings. The set covers storage, backup profiles, schedules, generated backup history, profile authoring, YAML import/export, one-time runs, scheduled Run now, details/download, schedule deletion, and server Maintenance restore flows.

The refined pass is intentionally minimalist and monitoring-oriented: the overview answers backup health first, failed or unavailable sources are surfaced as an explicit attention item, tables contain only decision-relevant columns, and authoring forms use numbered steps with a source-resolution preview before saving. Restore remains a server Maintenance action; creating a new server from a tracked bundle is shown in the server-creation flow.

Render or refresh all PNGs with:

```sh
node docs/mockups/issue-37/render.js
```

| PNG | View |
| --- | --- |
| `01-backups-overview.png` | Health-first operational overview with recent backups and upcoming schedules |
| `02-backup-profiles.png` | Backup Settings > Profiles with compact search/filter results and the resolved World save definition |
| `03-backup-profile-form.png` | Numbered profile form with logical items, source rules, and YAML import/export |
| `04-backup-schedules.png` | Schedule list, execution history, Run now actions, and bottom schedule menu |
| `05-backup-schedule-form.png` | Schedule form with automatic `.sav.backup` / `.sav` source selection |
| `06-run-backup-form.png` | One-time backup form and source preview |
| `07-backup-settings.png` | Backup Settings > Storage with the connected Local backend |
| `08-scheduled-run-now.png` | Immediate run of a saved schedule without changing its next run |
| `09-backup-details.png` | Completed backup details with bundle download only |
| `10-schedule-delete-confirmation.png` | Destructive schedule deletion confirmation from the bottom menu |
| `11-maintenance-restore-from-backup.png` | Server Maintenance restore modal using a tracked backup |
| `12-maintenance-restore-custom-sav.png` | Server Maintenance restore modal using a custom `.sav` file |
| `13-create-server-from-backup.png` | Server creation flow that restores a tracked bundle into a new world PVC |
