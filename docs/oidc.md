# Configure OIDC sign in

RSDW C2 supports one OIDC issuer with explicit admin and viewer assignments. Token authentication remains the default. Select one mode for the whole console.

## Register the console

1. Register a confidential web client with your identity provider. Enable authorization code flow with S256 PKCE and signed ID tokens.
2. Register the exact callback `https://console.example.com/api/auth/callback`. Replace the host with your public console host. The console always returns to `/` after sign in.
3. Obtain the issuer URL from the provider's discovery document. Use the exact `issuer` value, including any path. The console fetches `<issuer>/.well-known/openid-configuration`.
4. Assign exact issuer-local subject IDs or groups to the console roles. Configure your provider to include group membership as an array of strings in the signed ID token. Request a provider-specific group scope if required.
5. Store the client secret in an existing Kubernetes Secret. The values file references its name and key.

For Cloudflare Access, create a **SaaS application using OIDC**. Its issuer has the form `https://<team>.cloudflareaccess.com/cdn-cgi/access/sso/oidc/<client-id>`. Its discovery URL adds `/.well-known/openid-configuration`. Use the SaaS client ID and secret. A self-hosted Access application's audience tag and forwarded identity headers do not configure console OIDC. Confirm the issuer and claims with [Cloudflare's generic OIDC SaaS instructions](https://developers.cloudflare.com/cloudflare-one/access-controls/applications/http-apps/saas-apps/generic-oidc-saas/).

If the provider cannot include groups in the ID token, use subject assignments. RSDW C2 does not fetch UserInfo, infer roles from email addresses, or interpret upstream Access policies as console roles.

## Configure Helm

Place the console behind an HTTPS reverse proxy or tunnel. Forward requests to the console's private HTTP Service on port 8080. Preserve the browser's `Origin` header. Restrict direct access to the Service. The chart does not create an ingress, TLS certificate, or identity-provider application.

Save the following as your own values file. Replace the example values before installing.

```yaml
auth:
  mode: oidc
  adminTokenSecret:
    name: ""
  oidc:
    issuer: https://identity.example.com/realms/games
    publicOrigin: https://console.example.com
    clientID: dragonwilds-console
    clientSecret:
      name: rsdw-c2-oidc
      key: client-secret
    scopes: [openid, profile, groups]
    groupsClaim: groups
    rolePolicy:
      adminSubjects: []
      viewerSubjects: []
      adminGroups: [dragonwilds-operators]
      viewerGroups: [dragonwilds-viewers]
```

Run `helm template rsdw-c2 charts/rsdw-c2 -f your-values.yaml` to review the Deployment before installation. Use your normal release workflow to install it. The existing Secret must be available in the release namespace. Restart the Deployment after changing its Secret or role policy.

`publicOrigin` must be an HTTPS origin without a trailing slash, path, query, or fragment. Use an ASCII DNS hostname or a canonical IP address. The console lowercases its hostname, compresses IPv6 addresses, normalizes numeric ports, and removes the default HTTPS port to match the browser's Origin header. Scoped and IPv4-mapped IPv6 addresses are unsupported. Issuers must use HTTPS and cannot contain credentials, queries, or fragments. Discovery endpoints must use HTTPS but may include provider query parameters. There is no runtime setting to disable TLS or ID-token verification. The single replica and `Recreate` strategy are required because sessions live in process memory.

## Provider-specific setup

Use your identity provider's discovery document to confirm the issuer, scopes,
group claim, and callback URL. Map provider groups or subject IDs to the
generic role lists in the Helm values above. After deployment, test an admin,
viewer, and unmapped identity separately. A successful redirect alone does not
verify the client secret, token exchange, or role mapping.

## Configure without Helm

Set these environment variables on the console process. Supply the secret through your deployment's secret manager.

| Variable | Value |
| --- | --- |
| `RSDW_AUTH_MODE` | `oidc` |
| `RSDW_OIDC_ISSUER` | Exact HTTPS issuer |
| `RSDW_OIDC_PUBLIC_ORIGIN` | Public HTTPS origin |
| `RSDW_OIDC_CLIENT_ID` | Registered client ID |
| `RSDW_OIDC_CLIENT_SECRET` | Client secret |
| `RSDW_OIDC_SCOPES` | Space-separated scopes, default `openid profile` |
| `RSDW_OIDC_GROUPS_CLAIM` | Top-level ID-token claim, default `groups` |
| `RSDW_OIDC_ROLE_POLICY` | JSON object with `adminSubjects`, `viewerSubjects`, `adminGroups`, and `viewerGroups` arrays |

Clear `RSDW_ADMIN_TOKEN` in OIDC mode. Token mode rejects nonempty OIDC environment settings. Missing configuration, discovery failure, and an empty role policy prevent startup. `openid` is required; `offline_access` is unsupported.

## Verify access

Sign in with each intended role. Admins can create servers, read logs and events, restart servers, update images, and check for updates. Viewers can use the dashboard and telemetry, select servers and time ranges, pause or refresh, and export metric CSV files. Viewer responses omit server configuration, images, endpoints, owner and admin IDs, secret names, events, logs, and raw collection errors.

An identity with no exact assignment is denied. Admin takes precedence when an identity explicitly matches both roles. Subject assignments belong to the configured issuer. Group values are case-sensitive and must be in the verified ID token.

Sessions expire at the earlier of one hour after login and ID-token expiry. Polling does not extend them. Each new OAuth login gets a unique browser-binding cookie that lasts two hours, covering the five-minute login window and maximum session lifetime. An authenticated request to sign in redirects home without changing cookies. Authentication requires both the session cookie and the matching current binding cookie. A missing, malformed, duplicate, or mismatched binding denies access without revoking another browser's session.

Sign out revokes the current console session, including copied cookies, cancels pending transactions associated with the current binding, and clears both cookies. A new login cancels transactions associated with the previous binding supplied on its request. Independently started logins can have different bindings in one browser. Their callbacks may still finish, but callbacks never set or clear the binding cookie. A session cookie delivered by an already-in-flight callback after successful logout cannot authenticate without its original binding. This also holds when session admission finished before logout and only the response was delayed. Response reordering may lose access and require signing in again. Other browsers remain unaffected.

A delayed initial login response first delivered after logout can set its unique binding and start a new OAuth login. This does not revive an already-in-flight callback. Logout does not undo requests already authorized. A process restart revokes all sessions. Sign out does not end the identity provider's session, so the provider may sign the same person back in without a password prompt. Use the provider's account switch or sign-out flow to change accounts.

Both cookies are opaque, host-only, Secure, HttpOnly, and SameSite=Lax. Provider access and ID tokens are discarded after login. Authenticated writes require both the discovery response's CSRF token and the exact configured Origin. API and auth responses use `Cache-Control: no-store`. Do not cache `/api/*` at your proxy.

Pending logins expire after five minutes. The process permits at most 1,024 pending logins, 4,096 sessions, and eight concurrent token exchanges. It rejects admission when full without evicting active sessions. Provider requests have a ten-second timeout and a one-MiB response limit. Apply normal proxy rate limits to public login endpoints if exposing the console to the Internet.

## Run the local browser fixture

```sh
RSDW_OIDC_BROWSER_TEST=1 go test -run '^TestOIDCBrowserFixture$' -v -timeout 0
```

The test prints automatically assigned HTTPS URLs for the console, issuer, and session-expiration control. Open the issuer's `/expire` page first and accept its local test certificate. Then open the console and accept its certificate. Use **Sign in** and choose **Admin**, **Viewer**, or **Denied** at the test issuer. Use **Sign out** before choosing another role. Submit the issuer's expiration form to invalidate all fixture sessions, then refresh the console. Stop the test with Ctrl-C.

For an embedded browser that cannot accept local certificates, use the HTTP loopback mode:

```sh
RSDW_OIDC_BROWSER_TEST=1 RSDW_OIDC_BROWSER_HTTP=1 go test -run '^TestOIDCBrowserFixture$' -v -timeout 0
```

This prints HTTP URLs on `127.0.0.1` for the console and issuer's role picker and expiration control. The test keeps discovery, token exchange, JWKS, and verification on the TLS issuer with its trusted test CA. A test-only HTTP adapter renames the two auth cookies and removes their Secure flag for this browser mode. Production cookies still use their original names and flags. This mode does not verify production cookie TLS behavior; the TLS unit tests cover those attributes. Neither fixture variable enables an HTTP mode in the production binary.

Both modes run actual discovery, JWKS, signed ID tokens, and a PKCE-validating code exchange through production auth code. Only the Go test injects trust for its TLS certificate. The fixture serves the embedded UI with a fake orchestrator and cached numeric telemetry. It does not start Kubernetes, Docker, a game server, or an external identity provider. It does not establish that your production provider is configured correctly.
