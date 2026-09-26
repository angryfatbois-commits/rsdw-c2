# Clone a world

RSDW C2 can create a new server that starts as a copy of an existing world save. There are two sources:

- **A stopped server C2 still knows.** Any server with status `stopped` can be cloned from Maintenance.
- **A retained PVC from a completed keep-mode deletion.** Deleting a server with world data kept (the default) leaves a receipt on the Maintenance page. That receipt's retained volume can be cloned even though the original server no longer exists in C2's inventory.

Cloning a currently running (`online`, `starting`, or `attention`) world is out of scope for this version; stop the server first.

## What is copied

Only the world save file. Server password, admin password, and admin EOS IDs are never copied to the clone, the same way `createServerFromBackup` never copies them into a restore target. The new server gets its own identity, its own ownership token, and starts stopped so an operator can review it before bringing it online.

## Safety model

Both sources read through a short-lived, read-only pod mounting the source PersistentVolumeClaim, the same pattern the backup collector uses for a stopped server's world. The pod is deleted as soon as the read finishes or fails. C2 never writes to the source PVC and never mounts it read-write.

Kubernetes PVCs with `ReadWriteOnce` access, which is what the game chart provisions, reject a second concurrent mounter. If a backup or another clone is already reading the same PVC when a clone starts, the platform itself returns an error rather than allowing two readers to race; C2 does not add its own locking on top of that.

## API

```text
POST /api/servers/:id/clone
```

`:id` is either a stopped server's ID or a former server's ID that still has a completed, keep-mode deletion receipt.

Request body:

```json
{
  "serverName": "New world name",
  "ownerName": "Display name for the new server",
  "ownerId": "32-hex-character EOS player ID",
  "confirmCreate": true
}
```

`confirmCreate` must be `true`; it exists so the browser cannot fire this request without an explicit user action. On success the response is the newly created `Server`, already provisioned and stopped, mirroring `POST /api/backups/:id/create-server`'s response shape.

Failure modes:

| Condition | Status |
| --- | --- |
| Neither a server nor a completed keep-mode deletion receipt exists for `:id` | 404 |
| Source server exists but is not stopped | 409 |
| Source server is being deleted | 409 |
| Deletion receipt's world data choice was `purge` | 409 |
| Deletion receipt exists but its deletion has not finished | 409 |
| Retained world plan does not resolve to exactly one PVC | 409 |
| `serverName`, `ownerName`, `ownerId`, or `confirmCreate` fails validation | 400 |

If the source world save cannot be read, no new server is created and the request fails with 502. If the read succeeds but writing it into the newly created server fails, the new server is left in place, stopped, with an event describing the failure; retry the clone action from Maintenance rather than deleting and recreating it.
