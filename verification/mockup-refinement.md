# Mockup refinement validation

The September 16 refinement uses the supplied dashboard, telemetry, events, and maintenance mockups. Backups, integrations, and scheduled maintenance remain outside the first draft.

## Visual result

The UI uses charcoal surfaces, cobalt actions, a 66px header, a 233px sidebar, 40px desktop headings, 16px table text, and 4px panel corners. Telemetry has a main chart, two smaller charts, resource and health panels, metric definitions, and logs. Events retain selectable details. Maintenance groups lifecycle details, actions, readiness, recent changes, and audit events.

Browser inspection measured the header at 65.997px and sidebar at 232.998px at the mockup's 1586px viewport. Baseline and revised screenshots were inspected in the browser. This is visual alignment, not pixel-exact parity. Data, omitted features, header metadata, and the logo differ from the mockups.

## Browser checks

The local demo API backs these checks. No production server was changed.

| Controls | Observed result |
| --- | --- |
| Four navigation links, brand link, View server, View events, View all events | Correct page opened. Navigation resets scroll to the top. |
| Refresh, pause, resume | Timestamp updated; polling status toggled. |
| Server selector and All, Online, Needs attention | Selected context and server table changed. |
| Add server and empty-fleet create action | Create dialog opened. |
| Required fields, Cancel, close button | Empty required fields blocked submission; both dismissal paths closed the dialog. |
| Create confirmation | UI validation server appeared with the submitted eight-player limit and a creation event. |
| Telemetry range | Five-minute selection changed chart time labels. |
| Log search and refresh | WARN left one warning line; refresh changed its timestamp. |
| Telemetry CSV, log download, events CSV | Export actions reported 30 samples, filtered log output, and one filtered event respectively. |
| Event category buttons | System, Players, Health, Updates, and All each returned the corresponding events. |
| Event search | world returned World save committed. |
| Event selection and Copy event JSON | Details selected the update event; clipboard contained its expected ID, message, and details. |
| Check update | Added an update-check event and retained the available image. |
| Restart dialog and confirmation | Cancel left state unchanged; confirmation changed status to Starting and recorded Restart requested. |
| Update dialog and confirmation | Cancel dismissed; confirmation set image 0.1.2 and recorded Image update requested. |
| Sign-in close, cancel, invalid token, valid token | Dismissal preserved the locked state; invalid token showed an error; valid token opened an empty fleet. |
| Mobile navigation and create dialog | All four pages and the dialog worked at 390px without page-level horizontal overflow. |

Blob download completion is not exposed by this browser harness. The browser checks cover actions and notifications. A Node regression check separately verifies exact CSV content, quote escaping, and spreadsheet formula protection.

## Repeatable checks

`bash scripts/verify.sh` passes Go tests, Go vet, JavaScript syntax and rendering tests, Helm lint and template checks, and HTTP smoke requests. Its server uses an OS-assigned port and fresh state, so an existing preview cannot satisfy the checks.

`bash scripts/kind-smoke.sh` runs the real Docker build and creates an isolated one-node kind cluster. The test verifies readiness, authentication, empty inventory, in-cluster pod-exec permission, Helm creation, logs, restart, update revision, player-limit override, and audit output. The fixture is a small sleeping container, not the game. Cleanup removes the temporary cluster and its fixture.

Docker itself was healthy. The user session lacks access to its root-owned socket, so tests used existing sudo access. No account groups or daemon settings changed. The test uses a private kubeconfig and does not target the user's existing cluster.

## Limits

The run does not prove game startup, joining, world persistence, player-limit enforcement, or live resource telemetry. Those require a real game workload. Production telemetry and rollout-state gaps are tracked in [issue #1](https://github.com/petzkod5/rsdw-c2/issues/1). The UI does not add fabricated resource samples to cover missing sources.
