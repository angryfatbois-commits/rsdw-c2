# Create a server from a save

Select **Custom save** in the create-server form. Choose one nonempty `.sav` file up to 32 MiB, or leave the field empty to create a new world. The filename must end in lowercase `.sav`, contain no paths or control characters, not start with a dot, and fit within 128 UTF-8 bytes. C2 preserves the filename because the game importer uses it as the destination filename. Archives and directories are not supported. C2 treats save contents as opaque binary data. The game determines whether the save format is compatible.

The file limit is 33,554,432 bytes. The complete multipart request is limited to 33,619,968 bytes, including up to 32 KiB of JSON settings and multipart framing. C2 checks these limits before any staging write, including requests without `Content-Length`. Oversized uploads return HTTP 413. Type, filename, empty-file, and multipart errors return HTTP 400. A reverse proxy must allow the complete request size and enough time for upload and staging. For ingress-nginx, set `ingress.annotations.nginx.ingress.kubernetes.io/proxy-body-size` to `33m` and `proxy-read-timeout` to `300`.

The upload is part of the authenticated `POST /api/servers` request. It accepts exactly two multipart parts, `request` containing the existing create-settings JSON, and `save` containing one file. Token authentication and OIDC administrator permissions apply. OIDC requests also require the current session's CSRF header and origin. There is no reusable upload token or separate upload endpoint. The JSON-only create API is unchanged.

For a token-authenticated request:

```sh
curl --fail-with-body --max-time 300 \
  -H "Authorization: Bearer $RSDW_ADMIN_TOKEN" \
  --form-string 'request={"name":"Imported world","namespace":"dragonwilds","ownerId":"0123456789abcdef0123456789abcdef","maxPlayers":4}' \
  --form 'save=@World.sav;type=application/octet-stream' \
  https://c2.example.com/api/servers
```

## Storage and import

C2 persists seed ownership in `state.json` before staging. Binary content never enters state, logs, Helm values, or API responses. C2 writes a mode-0600 temporary file below `save-staging` next to the state file, then atomically renames it. The staging directory is private and filesystem access is confined to it. Filenames supplied by the browser never become local staging paths.

Each upload creates a separate 64 MiB `ReadWriteOnce` PVC and temporary writer pod in the game's namespace. These resources carry the `rsdw-c2.petzko.sh/seed` label. The pod runs as UID and GID 1000 with no service-account token, no privilege escalation, all capabilities dropped, and a read-only root filesystem. It copies the upload into a temporary file on the PVC, sets mode 0600, and atomically renames it. C2 deletes the writer pod before deploying the game. The writer has a 180-second deadline. Its default image is `busybox:1.37.0`; `RSDW_SEED_WRITER_IMAGE` can select a registry mirror with `sleep`, `sh`, `tar`, `test`, `chmod`, `sync`, and `mv -T`.

The cluster needs a default storage class that provisions the seed PVC and permits UID 1000 with `fsGroup: 1000` to write. Scheduling must allow the game's pod to mount both its world and seed PVCs. C2 uses the existing `kubectl` and Helm clients. The additional RBAC permission is `delete` for pods and PVCs, used for staging cleanup.

C2 supplies `saveSeed.existingClaim` and `saveSeed.path` to the game chart. The default chart version, `0.1.1`, mounts the seed read-only and runs `/opt/rsdwapi/seed-save.sh` as UID 1000. The importer rejects a source path that escapes the seed volume, including an escaped symlink. It copies only if the world directory has neither a `.sav` file nor `.seed-complete`. Subsequent restarts and image updates retain the seed reference and preserve the played world. Custom game charts and images must preserve this contract.

Keep C2's default persistent state volume enabled. The C2 chart disables uploads when `state.persistence.enabled=false`; empty-world creation remains available. Standalone C2 must use a durable `RSDW_STATE_FILE` and a writable parent directory. Demo mode cannot stage Kubernetes uploads. C2 accepts one create request at a time to bound upload memory and prevent duplicate release creation. A concurrent create receives HTTP 409 and can retry after the first finishes.

## Cleanup ownership

Before deployment succeeds, C2 owns cleanup through its persisted `pendingSeeds` records. It removes local staging files on request completion. Startup and a one-minute retry loop recover interrupted writes and abandoned requests. C2 deletes a failed request's seed PVC only after Kubernetes confirms that no Deployment or non-writer pod references it. A cleanup failure or uncertain Kubernetes response retains the ownership record for another attempt. A partial Helm deployment that still references the seed retains its PVC, even if the create request returned an error. C2 blocks another create with that release name until the pending record is reconciled.

After successful creation, the server's `saveSeed` metadata owns the PVC for the server's lifetime. C2 removes the local copy but retains the seed PVC regardless of the import marker. Restarts can still need to mount it. If C2 crashes after committing the server, the cleanup loop uses that server metadata to remove any remaining local copy. If the final state write fails, the earlier pending record continues to own the seed.

The operator owns cleanup of successful seeds after retiring the server. Remove the game's Helm release and confirm that its Deployment and pods no longer reference the exact seed claim. Remove the retired server's record from backed-up C2 state while C2 is stopped, then delete that seed PVC. Keep the separate world PVC if its saved world is still needed. Never delete a seed just because an import completed. For a failed partial deployment, remove its references to the seed and let C2's cleanup loop remove the pending PVC and record.

## Verification

`go test ./... -run TestSave -count=1` exercises authenticated API uploads, exact and excessive sizes, interrupted requests, invalid paths, atomic staging, Kubernetes manifests, deployment arguments, cleanup failures, persistence failures, and restart recovery with a simulated Kubernetes runner.

`node --test web/app.test.cjs verification/save-import.test.cjs` checks browser request construction, rendered RBAC and fixture storage, and the importer in fresh processes against persistent fixture directories. The fixture script is an unchanged copy of [the game image's v0.1.1 importer](https://github.com/petzkod5/rsdragonwilds-helm/blob/v0.1.1/container/seed-save.sh). These checks run in `bash scripts/verify.sh` without deploying to a cluster. They do not test game-engine parsing of an actual player's save.

The required verification script also runs `tests/users-browser.cjs` in Chromium. It checks file type and size errors, multipart submission to the demo API, the explicit Kubernetes-storage error, and successful empty-world creation after clearing the file selection.

Run `bash scripts/kind-smoke.sh` with Docker access to verify staging in an isolated cluster. The check uploads fixture bytes through C2, compares the imported file, verifies UID 1000 and read-only seed mounts, and checks that the writer Pod is gone. It changes the world file, restarts the server, and verifies that the replacement Pod preserves the changed world and retains the seed PVC. The fixture does not parse a real game save. The script deletes only its own temporary cluster when it finishes.
