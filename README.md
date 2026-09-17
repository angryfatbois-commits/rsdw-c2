# RSDW C2

RSDW C2 is a Kubernetes command and control console for RuneScape Dragonwilds dedicated servers.

The console supports these operations:

- Create a server with the `rsdragonwilds-helm` chart.
- Import an optional `.sav` file when creating a server. See [custom saves](docs/custom-saves.md) for upload limits and storage cleanup.
- Save named player IDs and select one when creating a server.
- Read live server health, player count, uptime, and logs; show resource metrics when a metrics source is available.
- Restart a server after confirmation.
- Detect image drift and push a new image tag after confirmation.

Backups and integrations are not part of this release.

## Run the console locally

Use demo mode to run the console without a Kubernetes cluster.

```sh
RSDW_DEMO_DATA=true RSDW_STATE_FILE=./state.json go run .
```

Open `http://localhost:8080`.

Demo mode loads one sample server so you can exercise the dashboard. A normal start uses an empty state file.

## Save player IDs

Open **Saved IDs** in the console to add, edit, or delete a named player ID. Copy your Player ID from the game's Settings menu, as described in the [official dedicated-server guide](https://dragonwilds.runescape.com/news/how-to-dedicated-servers).

In **Create a server**, choose a saved ID to populate **Owner EOS player ID**. You can edit that field or enter a one-off ID manually. The selector shows both the display name and player ID, so names can repeat.

Saved IDs and manual owner IDs must contain exactly 32 ASCII hexadecimal characters, `0-9`, `a-f`, or `A-F`. C2 converts uppercase hexadecimal letters to lowercase. It rejects empty values, whitespace, separators, and other characters without stripping them. This checks the identifier format, not whether the account exists. The same rules apply in demo mode.

Each canonical player ID can be saved once. Adding a duplicate or editing a record to use another record's player ID returns `409 Conflict`. Editing a record without changing its player ID is allowed. Deleting a record frees its player ID for reuse.

C2 stores records as `users` in its state file, keyed by stable record ID. Older state files load with an empty collection and retain their servers and events. Editing or deleting a saved ID never changes the owner ID already copied into a server.

Saved IDs require admin access. OIDC admins use their session cookie and the existing CSRF protection for changes. Viewers cannot list or change saved IDs. Token mode uses the existing admin bearer token, including explicitly configured demo tokens. Demo mode without a token allows local access.

| Method | Path | Body | Success |
| --- | --- | --- | --- |
| GET | `/api/users` | None | `200`, `{"users":[{"id":"...","name":"Alice","playerId":"..."}]}` |
| POST | `/api/users` | `{"name":"Alice","playerId":"0123456789abcdef0123456789abcdef"}` | `201`, saved record |
| PUT | `/api/users/{id}` | Both `name` and `playerId` | `200`, updated record with the same `id` |
| DELETE | `/api/users/{id}` | None | `204`, no body |

Names are trimmed and must be a single line of 1 to 48 bytes. Invalid input returns `400`; missing records return `404`. Player IDs are identifiers, not passwords or login credentials. These operations do not add player IDs to logs or events.

## Deploy the console

Choose a published version from [GitHub Releases](https://github.com/petzkod5/rsdw-c2/releases), then create the required admin token Secret.

```sh
VERSION=1.0.0 # Replace with the published version, without the v prefix.
kubectl create namespace rsdw-system
kubectl -n rsdw-system create secret generic rsdw-c2-admin \
  --from-literal=token="$(openssl rand -hex 32)"
helm upgrade --install rsdw-c2 oci://ghcr.io/petzkod5/charts/rsdw-c2 \
  --version "$VERSION" \
  --namespace rsdw-system \
  --set auth.adminTokenSecret.name=rsdw-c2-admin
```

The Service is a `ClusterIP`. Use a port-forward or put it behind your existing authentication proxy.

```sh
kubectl -n rsdw-system port-forward service/rsdw-c2-rsdw-c2 8080:8080
```

To create an Ingress, add these values to a file and pass it to Helm with `-f`.

```yaml
ingress:
  enabled: true
  ingressClassName: nginx
  annotations: {}
  hosts:
    - c2.example.com
  paths:
    - path: /
      pathType: Prefix
  tls:
    - secretName: rsdw-c2-tls
      hosts:
        - c2.example.com
```

The TLS Secret must contain `tls.crt` and `tls.key` in the release namespace. The UI uses root-relative asset and API URLs, so serve it at `/`. Subpath prefixes such as `/c2` are not supported.

The ServiceAccount can create and update the resources that the Dragonwilds chart uses. Token mode requires the admin token Secret. Install the console in a cluster or namespace reserved for these servers. Review the rendered `ClusterRole` before using it in a shared cluster.

If an authentication proxy sits in front of the Ingress in token mode, configure it to forward the browser's `Authorization: Bearer <admin-token>` header unchanged. RSDW C2 validates that bearer token against the admin token Secret.

For identity-provider sign in with admin and viewer roles, follow [Configure OIDC sign in](docs/oidc.md). OIDC mode requires HTTPS, an existing client Secret, and explicit role assignments. Its callback is `/api/auth/callback`. Viewers can read dashboard and telemetry data; admins can also read operational data and manage servers. OIDC mode does not accept the shared admin token or forwarded identity headers.

To upgrade, set `VERSION` to the next published version and rerun `helm upgrade --install` with the same Secret name. The packaged chart defaults to its matching image version. Do not override `image.tag` unless you intend to run a different image.

To inspect a release before installation, pull and render its package.

```sh
helm pull oci://ghcr.io/petzkod5/charts/rsdw-c2 --version "$VERSION"
helm template rsdw-c2 "rsdw-c2-$VERSION.tgz" \
  --namespace rsdw-system \
  --set auth.adminTokenSecret.name=rsdw-c2-admin
```

If the GHCR packages are private, authenticate with `helm registry login ghcr.io` and configure image pull credentials in the cluster. Repository maintainers can make both packages public in their GitHub package settings.

## Release versions and publication

The [CI workflow](.github/workflows/ci.yml) runs on pull requests and every push to `main`. Configure branch protection to require its `Verify` check. That job runs `bash scripts/verify.sh`, then offline tests of the workflow and publication guards. The image build runs after `Verify` for `linux/amd64`, including on pull requests, with publishing disabled. The image job loads that build into a disposable kind cluster and installs a packaged chart with an admin-token Secret. It requires readiness, rejection of unauthenticated requests, and successful authentication with the Secret's token before release can run. Its local `0.0.0` test package and image are never published.

CI uses Go `1.26.0` from `go.mod`, Node.js `24.10.0`, Python `3.13.7`, and Helm `4.2.2` on Ubuntu `24.04`. The image includes Helm `4.2.2` and kubectl `1.36.2`. Both cluster-check jobs explicitly install kind `0.33.0` and kubectl `1.36.2`. They use the digest-pinned Kubernetes `1.36.4` node image from the [kind release](https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0) and the hosted runner's Docker daemon. The release dependencies are pinned in `package-lock.json`, and Actions are pinned to commit SHAs.

After both checks pass for the same commit on `main`, semantic-release reads the full Git history and tags. It owns all release versions and creates exact `vX.Y.Z` tags. Do not create release tags manually. Supported Conventional Commit effects are:

| Commit | Release |
| --- | --- |
| `feat: ...` or `feat(chart): ...` | Minor |
| `fix: ...` or `perf: ...`, with any scope | Patch |
| A conventional revert with its original commit hash in the body | Patch |
| Any type with `!` or a `BREAKING CHANGE:` footer | Major |
| `docs`, `test`, `ci`, `build`, `chore`, `refactor`, or `style` without a breaking-change footer | None |

Use `!` or a `BREAKING CHANGE:` footer for breaking changes. With squash merges, put the intended Conventional Commit in the squash commit title and preserve the footer in its body. Without prior release tags, semantic-release starts at `1.0.0` when it finds release-worthy commits. A run with no release-worthy commits creates no tag or release and changes no tracked files. There are no version-bump commits or separate chart versions.

Publication runs in this order within the same gated workflow:

1. semantic-release creates `vX.Y.Z` and the GitHub release. Generated GitHub release notes are the project's changelog.
2. The publisher packages and checks the chart locally, then publishes `ghcr.io/petzkod5/rsdw-c2:X.Y.Z` only if that image does not already exist. It pulls the image, checks its source revision and architecture, runs the bundled Helm and kubectl, and starts the console with `RSDW_ADMIN_TOKEN` to check `/api/auth`. The run logs the immutable `ghcr.io/petzkod5/rsdw-c2@sha256:...` reference. There is no mutable `latest` tag.
3. After that exact image passes, the publisher pushes `oci://ghcr.io/petzkod5/charts/rsdw-c2` at version `X.Y.Z`. Packaging sets `version` and `appVersion` to `X.Y.Z`; the chart's existing default selects the same image tag. Source chart files stay unchanged.
4. The publisher pulls the OCI chart into a fresh directory and compares its contents with the locally packaged chart. It checks metadata, the default image, readiness, the required admin Secret reference, rejection without that reference, and a client-only Helm install dry run. CI also installs this downloaded archive in a fresh kind cluster with the matching image already pulled and a new admin-token Secret. It waits for the default persistent volume and Deployment to become ready, then checks the Service and authenticated API. Each check uses a private kubeconfig, prints diagnostics on failure, and deletes its disposable cluster. It never uses an external cluster.

Only the release job has `contents: write` and `packages: write`. It uses `GITHUB_TOKEN`; issue and pull-request writes are disabled. The workflow does not rely on a tag-triggered workflow, since tags created with `GITHUB_TOKEN` do not trigger another Actions run. See [GitHub's workflow trigger rules](https://docs.github.com/en/actions/how-tos/writing-workflows/choosing-when-your-workflow-runs/triggering-a-workflow).

If publication fails, rerun the failed Actions run for the same commit. A tag already at that commit supplies the original version. A missing GitHub release is recovered from that existing tag with GitHub-generated notes. An existing image must match the commit revision, and an existing chart must have identical extracted contents. Retries never repush existing versions; authentication, network, and content conflicts stop the run. The workflow serializes runs on `main` with `queue: max` and does not cancel an active publisher. [GitHub permits up to 100 pending runs per concurrency group](https://docs.github.com/en/actions/how-tos/write-workflows/choose-when-workflows-run/control-workflow-concurrency). A newer push does not repair an older incomplete release; rerun the older failed run explicitly.

Before the first release, ensure Actions can write repository contents and both GHCR packages, including access to any packages that already exist. A collision with a pre-existing version fails without replacing it. A GitHub release can appear before its artifacts finish publishing, so wait for its Actions run to succeed before installing it.

## Configure the server chart

The console uses these defaults.

- Chart: `oci://ghcr.io/petzkod5/charts/rsdragonwilds`.
- Chart version: `0.1.1`.
- Image repository: `ghcr.io/petzkod5/rsdragonwilds-server`.
- Namespace: `dragonwilds`.

Set `RSDW_CHART`, `RSDW_CHART_VERSION`, and `RSDW_IMAGE_REPOSITORY` on the console Deployment when the chart or image lives somewhere else.

The source chart requires an EOS owner ID and an existing API-token Secret. RSDW C2 creates the namespace and a release-specific Secret before it runs `helm upgrade --install`.

## Verify the project

Run the deterministic checks.

```sh
bash scripts/verify.sh
```

The checks cover Go tests, signed-token OIDC flows and rejection cases, the built service, the demo API, UI capability and stale-response tests, CSV tests, and positive and negative chart renders. Each run uses fresh state and automatically assigned ports. Run `go test -race ./...` for race checks. The [local OIDC browser fixture](docs/oidc.md#run-the-local-browser-fixture) supports independent browser testing without a cluster.

With Playwright and Chromium available, run `node tests/users-browser.cjs` after `bash scripts/verify.sh` for the authenticated Saved IDs browser flow. Set `NODE_PATH` if Playwright is installed outside the project, and `RSDW_TEST_CHROMIUM` to use a specific Chromium executable. The test uses temporary state and demo mode without a Kubernetes cluster.

The workflow has additional offline regression tests for its own publication logic. These use fake registry commands, real local Helm packaging, and the configured release-notes generator with a literal `feat(chart)` commit. They do not create Git tags, contact GHCR, or publish anything.

```sh
npm ci --ignore-scripts
node --test .github/release.test.mjs
actionlint .github/workflows/ci.yml
```

Actionlint `1.7.12` reports `queue` as unknown even though GitHub supports it. The workflow regression test covers `queue: max`; keep that setting when using an older linter.

`scripts/verify.sh` remains the application verification contract. The workflow tests supplement it; they do not replace or duplicate the application checks. A Docker build is an optional local check and also runs as a separate CI job.

```sh
docker build --platform linux/amd64 -t rsdw-c2:local .
```

To run the packaged-chart installation check locally, install the CI versions of kind and kubectl and enable Docker access. This optional local check runs automatically in CI and fails there if Docker is unavailable.

```sh
docker build --platform linux/amd64 -t ghcr.io/petzkod5/rsdw-c2:0.0.0 .
chart_dir=$(mktemp -d)
helm package charts/rsdw-c2 --version 0.0.0 --app-version 0.0.0 --destination "$chart_dir"
node .github/check-chart.mjs "$chart_dir/rsdw-c2-0.0.0.tgz" 0.0.0 --kind
```

Run the disposable low-memory cluster check when Docker access is available.

```sh
bash scripts/kind-smoke.sh
```

The script creates a disposable admin Secret inside the test cluster, deletes only the kind cluster that it creates, and exits with status 2 with `INCONCLUSIVE` when the Docker socket is unavailable.

The kind test uses a private kubeconfig. It verifies authentication, empty inventory, and create, logs, restart, and update operations against a small Helm fixture. It does not download or start the game image. The Max players field passes the chart's documented Unreal override; the game build determines whether that override is enforced.

See the [mockup refinement validation record](verification/mockup-refinement.md) for browser results and test limits. Remaining live-game telemetry and rollout-state gaps are tracked in [issue #1](https://github.com/petzkod5/rsdw-c2/issues/1).

The architecture and first-draft limits are in [ARCHITECTURE.md](ARCHITECTURE.md).
