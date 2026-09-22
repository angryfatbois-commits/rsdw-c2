# Issue 37 remediation

Done means production backup and restore write/read the expected bytes in isolated KIND, safety failures publish no backup or false restore success, profile/schedule edits preserve intent, repository failures preserve existing backups without leaking staged data, and the browser matches the approved mockups with each action exercised.

- [x] Read the poteto-mode principles and frame the scope against the issue and review.
- [x] Ground the existing profile, collector, repository, restore, and lifecycle contracts.
- [x] Repair collector safety and add executable regressions.
- [x] Repair repository recovery, limits, and runtime storage health.
- [x] Implement tracked/custom restore and new-server provisioning with target checks.
- [x] Repair profile validation, collisions, revisions, and schedule references.
- [x] Follow the mockup layouts and preserve profile/schedule edit round trips.
- [x] Verify through Go, browser, Helm, and a repeatable isolated KIND integration test.
- [x] Re-review the requirement map and record remaining evidence gaps honestly.

The user resolved the source convention: the observed `.sav.backup` is authoritative and the earlier `.bak` requirement was incorrect. Collection, restore mapping, UI, fixtures and documentation now use `.sav.backup`. See `docs/backups-verification.md` for current verification evidence and environment limits.

Throughput checkpoint: restore and lifecycle integration stay with the coordinator. Collector, repository/chart, and frontend patches use isolated copies with disjoint write sets. Existing state-store and lifecycle locks own mutations. The existing domain model remains the contract; no new architecture bakeoff is needed for the already-approved design. Restore adds a validated list of destination paths and bytes, staged on the target PVC and committed only while the actual game pods are absent.
