# OIDC and role design

The app already deploys as one replica. Use opaque, random session cookies backed by bounded process memory. This makes logout revoke the session without an encryption-key Secret or a persistent session service. A restart signs users out.

## Alternatives

| Design | Security | Logout | Provider compatibility | Maintenance | Viewer usability |
| --- | --- | --- | --- | --- | --- |
| A, opaque sessions with idle expiry | 5 | 5 | 5 | 3 | 2 |
| B, encrypted-cookie comparison leading to opaque sessions | 5 | 5 | 5 | 4 | 5 |

Scores use a five-point scale. Both independent candidates rejected encrypted cookies because immediate logout would still require server-side revocation state. B is the selected base because it keeps readable server names and uses absolute expiry. A's idle timeout adds no useful protection while the dashboard polls continuously. Candidates and judge used the inherited model, so this is review separation rather than cross-model coverage.

## Contract

- One concrete authentication owner. Named roles have a zero value that denies access. No authentication-provider adapter or session-store interface.
- OIDC code flow with S256 PKCE, single-use state bound to a browser cookie, nonce, and coreos/go-oidc token verification. Check authorized-party rules as well as issuer, audience, signature, and expiry. Discard provider tokens after login.
- Fixed HTTPS public origin determines callback and CSRF origin checks. Forwarded identity headers never authenticate a user. Production configuration cannot disable verification. A loopback browser fixture may inject test-only transport/configuration from a Go test, never from a production environment flag.
- Existing token mode remains backward compatible. OIDC mode rejects token fallback and mixed credentials. Explicit demo data may be used by the test fixture, but never bypasses OIDC enforcement.
- Explicit issuer-local subject and group mappings. Exact matches, admin precedence for explicit overlap, unmatched denial. No email-domain shortcut or automatic admin role. Groups must be present in verified ID-token claims if used.
- Absolute session lifetime is the earlier of one hour and ID-token expiry. No idle timer, refresh tokens, or sliding renewal. Prune bounded pending/session maps on admission and access; reject capacity overflow without evicting active sessions. Limit token exchanges and HTTP duration/response size.
- Authenticated mutations require both a per-session CSRF token and exact Origin. Login and callback use browser-bound state instead. Logout works for both roles and revokes a copied cookie. API authorization happens before body processing or orchestrator calls.
- Public auth discovery reports mode and sign-in/session information without credentials. Preserve probe compatibility. Authenticated discovery may return the session's CSRF token with no-store headers. Avoid a separate me endpoint unless needed.
- Viewer route allowlist contains only dashboard bootstrap and per-server telemetry. All operational events, logs, configuration, and mutations are admin-only, including check-update. Unknown routes default to denial.
- Viewer response types explicitly select server ID, display name, status, numeric metrics and timestamps. Retain static metric labels/units/definitions and safe cluster display metadata for useful navigation. Do not copy Server or arbitrary raw maps wholesale. Replace raw collector errors with fixed availability messages. Exclude owner/admin IDs, images, endpoints, secret names, settings, startup arguments and operational events.
- UI uses server capabilities, clears protected state on identity change/logout, and prevents stale in-flight requests from repopulating it. Viewer gets dashboard, telemetry selection/range/pause/refresh/CSV, role display and logout. No admin token prompt in OIDC mode.
- Helm selects exclusive auth modes, validates required settings and mappings, and references an existing client Secret. It remains single-replica and does not configure the user's IdP or TLS proxy.

## Verification and delivery

See [acceptance checks](oidc-acceptance.md). One implementation owner controls the auth, chart, and UI contract in this isolated worktree. The parent reviews the diff and runs independent browser/security verification after ownership returns. Preserve existing user changes in other worktrees. Keep the game and kind down.

Baseline checks on ecbdfed passed. `scripts/verify.sh` covered Go tests/vet, JavaScript checks, Helm lint/templates, build and local demo smoke. `go test -race ./...` passed. Neither check started kind or a game server.

The independent judge selected B, 19/25 versus A's 16/25. The judge agreed on readable viewer labels, absolute expiry, bounded opaque sessions, and fewer configuration abstractions. This confirms the parent's selection. The implementation uses the contract above, not every speculative limit or interface in the candidate sketches.
