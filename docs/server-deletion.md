# Server deletion

Admins can delete one server from **Maintenance**. The selector, fleet list, and lifecycle panel show the world name, with the stable server ID appended when world names repeat. Older records fall back to the stored name, then the ID. The confirmation dialog always includes the stable ID. Check that ID when several worlds have the same creator. Viewers cannot delete servers or read deletion receipts.

## Keep or purge

**Keep world data** is the default. Cancel closes the dialog without changing inventory. Confirming disconnects players, removes the selected Helm release and its managed resources, and removes the inventory entry only after cleanup succeeds. C2 retains the world PVC and any remaining uploaded source-save PVC in their existing namespace, recording their names and UIDs in the receipt. The uploaded source may be the only copy if import has not succeeded. Proven C2-owned direct Secrets are cleanup targets in either mode.

**Permanently delete world data** also deletes the selected release-owned world PVC and only proven-owned source-save PVCs. It requires the exact additional text `DELETE WORLD <stable-server-id>`. A PVC supplied through `persistence.existingClaim` is external to the release and cannot be purged through C2; use keep and leave disposal to its owner.

C2 never deletes the namespace. Other releases and unrelated resources remain. PVC deletion is not physical erasure: the storage provider's reclaim policy controls the backing PV, and snapshots or backups can survive. A `Retain` reclaim policy can leave the backing volume behind. Arrange any required disposal with the storage operator.

## Receipts and recovery

Maintenance shows completed and pending receipts after reload. Preserve C2's persistent state file; it holds deletion intent, the fixed keep/purge choice, resource identities, and completion status. Deleted identities are not automatically reused.

For a kept world, an operator can verify the receipt's PVC UID and exclusive use, then configure `persistence.existingClaim` with that PVC name in the same namespace. C2 does not automatically restore a deleted inventory entry. Recovery requires a separately configured server and any needed credentials.

The receipt lists uploaded source-save PVCs separately under `plan.seeds`. Verify the recorded UID and preserve the source for recovery through `saveSeed`, which imports the uploaded save into a world volume. Do not use the source-save PVC as `persistence.existingClaim`; that setting mounts an existing world volume. Keep the source until successful import and recovery are verified.

Unproven external or legacy direct Secrets are retained. Their identities appear in `plan.retainedSecrets` and in the receipt, without Secret values. C2 does not require manual ownership adoption or legacy ownership repair to complete deletion. Retaining these Secrets does not imply that they are safe to remove later; their owner must check other consumers. Secrets managed by another release remain that release's responsibility.

## Failure and retry

C2 records the plan before resource deletion. A partial failure leaves the server pending and blocks its other lifecycle actions. Inspect the receipt's error, address the reported condition, and choose **Retry deletion**. Retry uses the recorded identities and keep/purge choice; the choice cannot change. Purge retries require the same additional confirmation text. A completed operation returns the saved receipt when repeated with the same choice.

Do not remove the receipt or recreate resources under the deleted identity to get past a failure. C2 refuses replacement UIDs or a changed Helm release identity, revision, or manifest. Claims must be Bound, not terminating, and free of owner references. Shared storage, unexplained resources, release hooks, or missing retention protection can prevent preflight from completing. Have the operator investigate the specific error before retrying.

## Operating constraints

Run one C2 writer with persistent writable state. Lifecycle serialization is local to that process, not a distributed lock. Multiple C2 writers sharing inventory or managing the same releases are unsupported.

Exclude external Helm upgrades, rollbacks, uninstalls, and Kubernetes changes to the target release, workloads, storage, and Secret references throughout deletion and any pending retry. C2 checks identities and references, but these checks do not lock out external actors. Pause other reconcilers or coordinate a maintenance window before starting. A failed operation may already have removed some resources.

The API is `DELETE /api/servers/{id}` with JSON `{"confirm":"<id>","mode":"keep"}`. Omitting `mode` defaults to keep. Purge requires `"mode":"purge"` and `"purgeConfirm":"DELETE WORLD <id>"`. Admin authentication and the existing OIDC CSRF rules apply. Successful requests return the receipt with `200`.

## Verification boundary

`node tests/deletion-browser.cjs` runs Chromium against the actual app with a local demo state file, plus the existing OIDC fixture for viewer denial. It checks labels, selection, cancellation, confirmation, inventory isolation, receipt persistence, and retained Secret rendering. Its saved storage identities are rendering fixtures only. Demo mode does not touch Kubernetes and cannot prove PVC retention or deletion. Those guarantees require separate acceptance against an actual cluster.
