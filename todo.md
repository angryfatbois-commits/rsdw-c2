# RSDW C2 autonomous run

## Figure it out playbook

- [x] Open a todolist whose first item is to read the Principles section of the poteto-mode skill. Then add the phases below as todos.
- [x] Phase A: Frame. State the definition of done as a falsifiable predicate, quantify scope, and set rigor from blast radius.
- [x] Phase B: Design the workflow. Decompose into atomic units, build verification before features, and write the phase list down.
- [x] Phase C: Run the loop. Treat each unit as an experiment and record VERIFIED, NOT VERIFIED, or INCONCLUSIVE.
- [x] Phase D: Keep the audit trail. Log decisions and checkpoints in one canonical TSV.
- [x] Phase E: Verify and hand back. Check the real product against the predicate and encode recurring corrections as gates.

## Framing

- [x] Definition of done: the GitHub repository exists, the API and UI build, each visible control has an exercised behavior, Helm templates pass lint/render checks, the control plane can run in demo mode, and a low-memory Kubernetes deployment check passes or is recorded as inconclusive with the blocker.
- [x] Scope: one Go service, one static web console, one control-plane Helm chart, one deterministic verification script, one browser interaction pass, and one low-memory Kubernetes test attempt.
- [x] Rigor: high. The app can restart or mutate live game servers, so deployment paths, confirmation dialogs, RBAC, input validation, and audit events need direct checks.

## Designed phases

- [x] Ground source chart and choose the smallest data model.
- [x] Build the verification lever and local app scaffold.
- [x] Implement API state, persistence, and Helm/Kubernetes commands.
- [x] Implement the dashboard, telemetry, events, and maintenance flows.
- [x] Add the control-plane Helm chart and RBAC.
- [x] Verify in small units, then run browser and kind checks.
- [ ] Review the diff, commit the audit trail, and push the repository.

## Feature throughput checkpoint

- [x] Blocking first steps. Read the source chart, load the UI design system, create the repository, and define the domain model before fan-out.
- [x] Independent workstreams. Design review ran in parallel; implementation stays in one owner because the API model, UI actions, and Helm values share one contract.
- [x] Shared mutable state. Keep the JSON state file and generated Helm values owned by the single Go service; no concurrent writers are introduced.
- [x] Smallest safe decomposition. One implementation owner is safest because every UI action must map to one API behavior and the deployment contract is cross-cutting.

## Verification units

- [x] Unit 1. Domain validation and JSON persistence.
- [x] Unit 2. HTTP API and demo-mode action behavior.
- [x] Unit 3. Static UI route and interaction matrix.
- [x] Unit 4. Helm lint and rendered deployment/RBAC manifest checks.
- [x] Unit 5. Browser validation of every button, selector, chip, and modal.
- [x] Unit 6. Low-memory kind deployment check.
