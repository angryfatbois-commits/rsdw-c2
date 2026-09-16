# UI validation record

This matrix records the browser pass against the local demo server. Each listed control was exercised through the rendered UI and its visible state, route, toast, modal, or API-backed data was checked.

| Area | Control | Observed result |
| --- | --- | --- |
| Global | Dashboard, Telemetry, Events, Maintenance navigation | Each route rendered the selected page and preserved the server context. |
| Global | Refresh data | Re-fetched the demo state and updated the visible timestamp. |
| Global | Connection-loss refresh | After stopping the local server, refresh showed `Connection issue · data may be stale` and preserved the last known data. |
| Global | Initial outage retry | A simulated bootstrap outage rendered `Unable to connect`; Try again re-issued the request and kept the failure state visible. |
| Dashboard | Pause/resume live updates | Status changed between paused and connected states. |
| Dashboard | All, Online, Needs attention filters | Selected chip changed and the server table filtered accordingly. |
| Dashboard | Add server, cancel, confirm | Modal opened, cancel closed it, and demo confirmation created a second server and event. |
| Dashboard | Modal close X | Closed the create-server dialog and returned focus to Add server. |
| Dashboard | Server selector | Selecting the created server scoped the dashboard. |
| Dashboard | View server | Opened the selected server's telemetry page. |
| Telemetry | 60 seconds/5 minutes selector | Chart labels and range state changed to the selected window. |
| Telemetry | Log search | WARN search narrowed the log output. |
| Telemetry | Refresh logs | Refreshed the log data and showed a success toast. |
| Telemetry | Export telemetry/logs | Both actions produced their export-ready success toasts. |
| Events | Search events | `world` reduced the visible event set, and clearing it restored the set. |
| Events | Category chips | System selected system events; Updates produced an empty filtered state. |
| Events | Event row and Copy event JSON | Details opened, the selected row state changed, clipboard JSON was readable, and a success toast appeared. |
| Events | Export CSV | Produced an export success toast. |
| Maintenance | Check update | Added an update-check event and exposed the image drift result. |
| Maintenance | Restart, cancel, confirm | Confirmation appeared; cancel closed it; confirm changed status to Starting, updated restart time, and added an event. |
| Maintenance | Update image, cancel, confirm | Confirmation appeared; tag `0.1.2` deployed in demo mode and updated current/desired image state. |
| Empty state | Create first server, cancel | Empty-state action opened the create modal and cancel returned to the empty state. |
| Auth | Invalid and valid admin token | Invalid token showed an error; the configured token unlocked the dashboard. |
| Auth | Close sign-in X and Cancel | Both closed the sign-in dialog without changing the locked state. |

The browser download event itself is not exposed by the in-app browser harness for blob URLs; the export handlers were still validated by their immediate success toasts and state transitions.
