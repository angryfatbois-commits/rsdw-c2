# Dragonwilds Control design system

The user PNGs in `/home/petzko/dragonwilds-admin-mockups/admin-pages/v2/` define the visual direction. The supported pages are Dashboard, Telemetry, Events, and Maintenance. Backups and Integrations are outside this implementation.

## Colors and typography

| Role | Value |
| --- | --- |
| Page background | `#171f25` |
| Card background | `#1d272e` |
| Primary action and active navigation border | `#0063ff` |
| Primary text | `#f5f7fa` |
| Secondary text | `#b4c1d0` |
| Links | `#69afff` |
| Dividers | `#2d3a44` |
| Control borders | `#52616d` |
| Success status | `#32d766` |
| Warning status | `#ffc329` |
| Destructive action and error status | `#ff5d66` |

Green appears only on successful or healthy status dots. Status text remains white. Actions, chart lines, selections, and navigation use blue. Status text accompanies every status dot.

The font stack is Inter, Segoe UI, Arial, sans-serif. Fonts load from the device with no external request. Page titles are 40px on desktop and 32px on mobile. Table cells and body text are 16px. Logs and JSON use the system monospace font.

## Layout

The desktop header is 66px high. The sidebar is 233px wide. Main content has 30px horizontal gutters and 24px top padding. Cards have 4px corners, 20px vertical padding, and 24px horizontal padding. Gaps between cards are 16px.

The sidebar contains the four supported page links. The header shows the product name, connected cluster, and environment. Page headings contain the server selector, refresh and pause controls, and update time. Connection status appears below the content.

At widths below 1280px, page controls wrap below the title. Below 1050px, split layouts become one column and statistics use two columns. Below 760px, navigation becomes a horizontal row and main gutters shrink to 16px. Tables scroll within their cards. Their text remains 16px.

## Pages

- Dashboard follows `01-dashboard.png`. It displays server statistics, the server table with status filters, and fleet activity. The fourth statistic shows supported update availability instead of an unavailable backup timestamp.
- Telemetry follows `02-telemetry.png`. Tick rate occupies the large left card, with player count and network traffic below. Resource usage and health checks occupy the right column. Metric definitions and logs span the full width below. Network lines use the response's inbound and outbound sample fields. Definitions come from `metricDefinitions`.
- Events follows `03-events.png`. Search and export precede event statistics. The event stream and category filters sit beside event details. Selecting an event preserves focus on its row button.
- Maintenance follows `06-maintenance.png`. Server lifecycle sits beside readiness and recent changes. The audit trail spans the full width. Recent changes show recorded system and update events. The audit trail shows server events and states that actor identity is unavailable.

## Data and interaction constraints

Missing metrics show an unavailable value or an explicit empty state. The UI does not invent previous-period comparisons, p95 timing, backup state, maintenance windows, actor identities, or stream-health claims.

Existing state handlers and test IDs remain in place. Supported lifecycle actions are restart, update image, and check update. Restart and update retain confirmation dialogs.

## Accessibility

Controls have a 44px minimum height. Keyboard focus uses a white 2px outline. Table wrappers include space for focus outlines. Native dialogs retain their labels, error announcements, and focus handling. Scrollable logs and event JSON are keyboard focusable. Reduced-motion preferences disable transitions and animations. Charts expose series names and latest values through accessible labels.

Browser verification and baseline capture belong to the coordinating agent. Syntax and Go checks alone do not establish pixel parity.
