# OIDC implementation verification

The implementation adds exclusive token and OIDC modes, explicit admin and viewer roles, bounded opaque sessions, viewer data projections, capability-driven UI, and Helm configuration. The callback is `<public-origin>/api/auth/callback`.

## Automated results

The implementation owner ran these checks in the `codex/oidc` worktree on 2026-09-16.

| Check | Result |
| --- | --- |
| `bash scripts/verify.sh` | Passed after the HTTP browser fixture was added at `bb0453c`. Includes Go tests and vet, JavaScript syntax and tests, build, Helm lint and renders, and local demo smoke. |
| `go test -race ./...` | Passed at `bb0453c`. |
| `go test -race -run 'TestReviewLogoutDuringCallback\|TestOIDCNewLoginSupersedesInFlightCallback' -count=3 .` | Passed after the logout ordering fix. |
| `go test -race -run 'TestOIDCHTTPBrowserAdapter\|TestReviewLogoutDuringCallback\|TestOIDCNewLoginSupersedesInFlightCallback' -count=3 .` | Passed after the HTTP adapter was added. |
| `go test -race -run TestOIDCCanonicalPublicOrigin .` | Passed at `8073678` for hostname case, default HTTPS port, nondefault port, callback construction, and logout Origin checks. |
| `node --test web/app.test.cjs` | Passed for rendering, CSV, viewer capability filtering, identity cleanup, late response rejection, pending logout, expired-session logout, and legacy-token discovery retry. |
| `bash scripts/verify-auth-chart.sh` | Passed token/OIDC positive cases and missing, mixed, HTTP, invalid origin, empty policy, scope, replica, and unknown-option rejection cases. |
| `git diff --check` | Passed before commits. |

The TLS issuer tests exercise discovery, JWKS, RSA-signed ID tokens, PKCE code exchange, browser binding, state replay, nonce, issuer, audience, authorized party, expiry, signature/key rejection, and missing ID tokens. They also check explicit role policy, CSRF and exact Origin, cookie flags, logout revocation, capacity, nonrenewing expiry, process restart, query/cookie ambiguity, and fail-closed configuration.

Route tests deny anonymous access to all protected routes and restrict viewers to the two allowed read shapes. They check that denied requests do not invoke the orchestrator or change stored state. Viewer projections exclude secret markers from server settings, events, arbitrary metrics, and collection errors, and reject nonfinite numeric values.

## Logout ordering regression

The independent review reproduced an earlier admin callback minting a session after a newer viewer login and logout. `TestReviewLogoutDuringCallback` preserves that reproduction. `TestOIDCNewLoginSupersedesInFlightCallback` adds realistic shared-browser cookies, callback replay while exchange is blocked, logout without the cleared login cookie, replacement without logout, and an unaffected second browser.

Transactions now remain stored as consumed during exchange. A newer login or logout cancels them. Session admission rechecks the transaction under the same mutex used for cancellation. Sessions retain their browser lineage. This resolves the reviewed ordering bug without global user revocation.

## Browser fixture and limits

Run the HTTP loopback fixture when the browser cannot accept a test certificate:

```sh
RSDW_OIDC_BROWSER_TEST=1 RSDW_OIDC_BROWSER_HTTP=1 go test -run '^TestOIDCBrowserFixture$' -v -timeout 0
```

The test prints the console and test issuer URLs. Choose Admin, Viewer, or Denied during sign in. The issuer's `/expire` form invalidates fixture sessions. Ctrl-C stops the process. Omit `RSDW_OIDC_BROWSER_HTTP=1` for the TLS browser mode.

The HTTP mode changes only test code. Its browser pages use HTTP and translated cookies; discovery, token exchange, JWKS, and token verification retain TLS with the test CA. This mode does not verify production Secure-cookie behavior. TLS unit tests verify the production attributes.

The fixture uses the real embedded UI, auth implementation, fake orchestrator, and cached numeric metrics. No Docker, kind, game server, live provider, or production system was used. Production IdP setup remains a deployment task.

## Independent verification

The parent ran `go test -race -count=1 ./...` and `bash scripts/verify.sh` at `7b2653b`. Both passed. An earlier independent `govulncheck` run found no vulnerabilities in the feature dependencies.

Both commands passed again after the final public-origin regression fix and telemetry description edit. The race run completed in 5.759 seconds. The temporary browser fixture was stopped after verification.

Browser checks used the in-app browser with the user-approved HTTP loopback fixture. Production HTTPS and browser certificate settings were not changed.

| Browser check | Observed result |
| --- | --- |
| Anonymous entry | Sign in required; no admin-token prompt or protected data. |
| Unmapped identity | Sign-in failure message; no session or navigation. |
| Admin login | Dashboard, telemetry, events, maintenance, and Add server controls visible. |
| Admin restart | Confirmation dialog; Confirm restart produced Server restart requested against the fake orchestrator. |
| Sign out | Protected data and navigation cleared; sign-in screen remained. |
| Admin-to-viewer switch | Viewer role, dashboard and telemetry only; no create, maintenance, events, or logs. |
| Direct viewer maintenance URL | Dashboard rendered instead; no lifecycle controls. |
| Viewer telemetry | Synthetic player, tick, CPU, memory, and traffic charts rendered. Five-minute and one-hour selection worked. |
| Pause, resume, refresh | Pause changed to Resume updates; resume restored polling; refresh completed. |
| CSV export | UI reported Exported 80 records; no browser console errors. The browser-tool download event timed out, so file delivery is not claimed. CSV serialization passes automated tests. |
| Session expiry | Fixture expiry followed by refresh cleared protected data and returned to sign in. |
| Session persistence | Reload retained the viewer session. |
| Mobile viewport | Not verified. The browser ignored the requested 390-pixel viewport and retained a 1265-pixel content width. The temporary override was reset. |

The browser run did not deploy a game server, test a live identity provider, or prove production Secure-cookie transport. Automated TLS tests cover production cookie attributes and OIDC validation. Browser inspection found a description that promised logs to viewers; the shared telemetry description now refers only to performance and resource usage.

Final independent review found no residual defect in callback cancellation. It found that padded ports and expanded IPv6 origins could fail exact Origin checks. The added regression failed before the fix and passed after port and address normalization. Invalid ports, scoped IPv6, and IPv4-mapped IPv6 now fail configuration checks. Issues #13 and #14 track these pre-merge findings. The review also corrected two claims in the design document. Review agents used the inherited model, not a separate model family.

## Changed files

- Authentication and data projection: `auth.go`, `auth_test.go`, `viewer.go`, `main.go`, `main_test.go`, `telemetry_test.go`, `go.mod`, and `go.sum`.
- Browser UI and checks: `web/app.js`, `web/app.test.cjs`, `web/index.html`, and `web/styles.css`.
- Deployment and verification: `charts/rsdw-c2/values.yaml`, `charts/rsdw-c2/values.schema.json`, `charts/rsdw-c2/templates/deployment.yaml`, `scripts/verify-auth-chart.sh`, and `scripts/verify.sh`.
- Documentation: `README.md`, `ARCHITECTURE.md`, `docs/oidc.md`, and this record.

The local `todo.md` is not part of the feature commits. The decision record is in `.audit/oidc-decisions.tsv`.
