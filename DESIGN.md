# DESIGN.md — RSDW C2

Agent-supplied direction (antislop wizard step 3, option 2). Written before any UI code changed. Honest risk: agent-authored direction skews toward default AI taste; every decision below carries a one-line reason to counter that, per R-31.

## Product identity

RSDW C2 is a self-hosted operations console for Dragonwilds dedicated game servers on Kubernetes. One operator (and a small group of trusted admins) monitor server health, respond to alerts, and perform maintenance. It is not a public SaaS product, not a marketing site, and not aimed at strangers evaluating whether to sign up.

**Audience**: technical operator, at a desk, often for extended monitoring sessions, sometimes reacting to an alert at 2am. Optimized for fast scanning of real-time state, not persuasion.

**Personality**: a beacon watchtower, not a corporate dashboard. Calm most of the time, unambiguous the moment something needs attention.

## Design Read

> Reading this as: operations console for a technical solo/small-team operator, in a Vercel/Stripe-modern visual language (user-selected anchor), dial ENERGY 2 / RHYTHM 2 / MOTION 2.

- **ENERGY 2**: confident contrast and one strong accent color, not shouting, not sterile.
- **RHYTHM 2**: consistent grid with deliberate breaks where a screen's real job differs (e.g. the server overview's one hero metric vs. the dashboard's server list).
- **MOTION 2**: transitions on hover, focus, and modal open/close communicate interactivity; nothing loops or runs without a trigger.

## Palette

| Token | Value | Reason |
|---|---|---|
| `--bg` | `#14171B` (warm near-black slate) | Dark is justified (R-21): this is a developer/ops tool used for extended sessions; warm undertone (not blue-black) avoids the generic "tech blue" SaaS default. |
| `--surface` | `#1B1F24` | One step up from bg for cards; flat, no gradient. |
| `--surface-raised` | `#22272E` | Reserved for the one genuinely elevated layer: modal/dialog. |
| `--border` | `#2C323A` | Structural separation, not decoration. |
| `--text` | `#F2F0EA` (warm off-white) | Pairs with warm base; avoids cold blue-white. |
| `--text-muted` | `#9BA3AD` | Secondary reads, meets 4.5:1 on `--bg`. |
| `--accent` | `#E8883A` (copper/beacon amber) | Core identity color. Ties to Dragonwilds' wilds/beacon-fire setting without literal game iconography (R-01: purpose stated, not a default gradient). Distinct from the generic blue-tech SaaS default the previous UI used. Used only at the key moment: primary actions, active nav, the one hero metric per screen. |
| `--on-accent` | `#14171B` | Text on accent backgrounds uses dark bg color, not white — checked with the contrast tool: white-on-copper is 2.3:1 (FAIL), dark-on-copper is 6.86:1 (PASS AA). |
| `--success` | `#3DBD6B` | Real state only: server online, action succeeded. |
| `--warning` | `#E0A93B` | Real state only: pressure/attention conditions. |
| `--danger` | `#E5555F` | Real state only: destructive actions, errors, offline. |

Three core neutrals (bg/surface/border-text family) + one accent (copper) + three functional status colors that mark real state, not decoration. Satisfies R-29 (2-3 core + 1 accent; status colors are functional, not "palette" colors).

## Typography

- **UI text**: Inter — chosen because the product already ships dense tabular data (player lists, metric grids, event logs) and Inter's numeral spacing and x-height hold up at small sizes; kept from the prior version deliberately, not by default (R-06).
- **Technical values**: JetBrains Mono, scoped ONLY to server IDs, endpoints, release names, and raw metric numbers (tick rate, byte counts) — reason: monospace aids column alignment and copy-paste accuracy for values operators paste into `kubectl`/scripts. Not used as a decorative "hacker" aesthetic on headings or body copy.
- Heading weight carries hierarchy (700/650/600 across h1/h2/h3), not size alone.

## Shape & elevation

- **Radius**: 8px on cards/panels/modals, 6px on inputs/buttons/badges — one small consistent scale, not pill-everywhere (R-11).
- **Shadow**: none on cards (flat, matches an admin-panel convention where every panel is equally "on the desk"); shadow reserved for the modal dialog only, because it is the one element genuinely floating above the page (R-12, elevation reason stated).
- **Glass/blur**: none. Flat surfaces read faster for an ops tool scanned under time pressure (R-10 dose cap: zero is a valid choice, not a default).

## Identity motif

A single **beacon dot**: a small solid circle that marks real server/connection state only (online = success color, attention = warning, offline = muted gray). No glow, no pulse loop — a static, factual mark (R-19, R-31: decorative-dot pattern explicitly rejected unless it marks real state). This is the one repeated, specific gesture that makes the UI "belong" to RSDW C2 rather than any generic dashboard.

## Layout philosophy

- Keep sidebar + topbar shell — it is a real functional convention for a multi-page ops console the operator uses daily, not swapped without reason (C-3).
- **Deferred to a follow-up, not this pass**: killing the "identical stat tiles" pattern on Dashboard/Server overview requires changing markup structure that 15+ Playwright browser tests assert against directly (counts, text content, screenshots). This pass ships the full visual system (palette, type, radius, shadow, motion, login scene, beacon-dot) with zero markup/class changes, so every existing test contract holds. The IA change is real and worth doing, but it is a separate, explicitly-approved change, not bundled into a visual rebrand.
- Dashboard's job: "which servers need attention right now." Server overview's job: "is this one server healthy, and what can I do to it." Both keep their current stat-row shape for now; only the visual system changes.

## What stays from the current build (not slop, keep it)

- Dark theme (justified above, not a default).
- Sidebar navigation structure (real, used daily).
- Accessible focus rings, skip link, `aria-live` regions — keep and extend, do not regress accessibility while restyling (antislop-human concern).

## Known follow-up: stale doc screenshots

`docs/backups.md`, `docs/telemetry.md`, `docs/integrations.md`, `docs/custom-saves.md`, and `docs/backups-verification.md` link screenshots under `docs/screenshots/` that still show the pre-rebrand blue-orange-purple palette. Layout and flow in those images are still accurate; only colors are stale. Regenerating them requires the Playwright doc-screenshot capture path (`RSDW_BACKUP_SCREENSHOTS=... node tests/backups-browser.cjs` plus equivalents for telemetry/integrations/custom-save), which needs a full Linux build + Playwright Chromium toolchain not available in this pass. Tracked here rather than silently left stale.
