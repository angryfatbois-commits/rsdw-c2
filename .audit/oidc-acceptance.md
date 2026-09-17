# OIDC acceptance checks

The production deployment and identity-provider configuration are outside this change. Do not start a game server or kind cluster for authentication tests.

## Login and sessions

- Run authorization-code login through an actual local test issuer with discovery, JWKS, signed ID tokens, and a token endpoint. Do not replace the app's authentication responses with browser mocks.
- Verify PKCE S256, state and browser binding, nonce, issuer, audience, signature, expiry, and required subject. Reject callback replay and malformed or expired transactions.
- OIDC mode does not accept the legacy admin token or forwarded identity headers. Incomplete configuration and unavailable discovery fail closed.
- Session cookies are HttpOnly, Secure in production, SameSite=Lax, host-only, and time-bounded. No identity-provider token reaches browser storage or API JSON.
- Logout revokes the session. Expired sessions cannot read or mutate. Fixed public origin prevents Host or forwarded-header callback poisoning and open redirects.
- Cookie-authenticated writes require CSRF protection and the configured origin. Test missing/wrong tokens, absent or foreign origins, cross-site requests, and logout.
- Authentication errors do not include client secrets, authorization codes, provider responses, or raw tokens.

## Authorization

- Explicit admin and viewer group/subject policy, derived only from verified identity claims. Unmatched identities cannot enter. Admin takes precedence for explicitly overlapping assignments.
- Admin can create, restart, update, check updates, read logs and operational events.
- Viewer can read the dashboard and telemetry. The returned server object contains no owner/admin IDs, secret names, startup arguments, full settings, or raw operational error text.
- Viewer cannot read logs/events or invoke any mutation, including check-update. Unknown or newly added routes are denied unless permitted deliberately.
- A denied request never calls the orchestrator or changes the store. Tests cover every current route, not just the UI controls.

## UI and chart

- Browser checks cover logged-out state, admin login, viewer login, denied identity, logout, expired session, navigation, and mobile layout.
- Viewers see their role and retain server selection, telemetry range, pause/refresh, and metric CSV export. No create, maintenance, logs, or operational-event controls remain.
- All calls use the same server-provided capabilities. Direct navigation to restricted pages cannot reveal stale admin data.
- Legacy token login remains usable when configured. OIDC users never see the admin-token prompt.
- Helm renders token and OIDC modes, references an existing client Secret, validates required OIDC settings/role policy, and rejects ambiguous or unsafe configuration. No literal client secret appears in a values example.
- Document generic discovery setup, exact callback URL, role claims, group scopes, logout/expiry behavior, TLS termination, and the Cloudflare Access SaaS OIDC issuer format.

## Regression checks

- Run the existing verification script, Go race tests, frontend tests, build, Helm lint/template positive and negative cases.
- Capture test output and browser observations in the verification record. State fixture limitations explicitly. A local issuer test does not prove that the user's unconfigured production provider is connected.
