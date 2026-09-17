#!/usr/bin/env bash
set -euo pipefail

version=${1:?release version is required}
[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]
[[ "${GITHUB_EVENT_NAME:-}" == push && "${GITHUB_REF:-}" == refs/heads/main && "${GITHUB_REPOSITORY:-}" == petzkod5/rsdw-c2 ]]
[[ $(git rev-parse HEAD) == "$GITHUB_SHA" && $(git rev-parse "refs/tags/v${version}^{commit}") == "$GITHUB_SHA" ]]

image="ghcr.io/petzkod5/rsdw-c2:$version"
chart=oci://ghcr.io/petzkod5/charts/rsdw-c2
task_dir=$(mktemp -d)
export HELM_CONFIG_HOME="$task_dir/helm-config" HELM_CACHE_HOME="$task_dir/helm-cache" HELM_DATA_HOME="$task_dir/helm-data"
container=
cleanup() {
  if [[ -n "$container" ]]; then
    docker logs "$container" || true
    docker rm -f "$container" >/dev/null || true
  fi
  rm -rf "$task_dir"
}
trap cleanup EXIT

helm package charts/rsdw-c2 --version "$version" --app-version "$version" --destination "$task_dir"
archive="$task_dir/rsdw-c2-$version.tgz"
node .github/check-chart.mjs "$archive" "$version"

if docker manifest inspect "$image" >"$task_dir/image.json" 2>"$task_dir/image.err"; then
  printf 'Reusing existing image %s\n' "$image"
else
  cat "$task_dir/image.err" >&2
  grep -Fq "no such manifest: $image" "$task_dir/image.err"
  docker buildx build --platform linux/amd64 --provenance=false \
    --label "org.opencontainers.image.source=https://github.com/petzkod5/rsdw-c2" \
    --label "org.opencontainers.image.revision=$GITHUB_SHA" \
    --tag "$image" --push .
fi

docker pull --platform linux/amd64 "$image"
revision=$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image")
if [[ "$revision" != "$GITHUB_SHA" ]]; then
  printf 'Refusing image %s: revision %s does not match %s\n' "$image" "$revision" "$GITHUB_SHA" >&2
  exit 1
fi
[[ $(docker image inspect --format '{{.Os}}/{{.Architecture}}' "$image") == linux/amd64 ]]
digest=$(docker image inspect --format '{{index .RepoDigests 0}}' "$image")
[[ "$digest" =~ ^ghcr.io/petzkod5/rsdw-c2@sha256:[0-9a-f]{64}$ ]]
printf 'Immutable image: %s\n' "$digest"
docker run --rm --entrypoint helm "$digest" version --short | grep -F 'v4.2.2'
docker run --rm --entrypoint kubectl "$digest" version --client=true -o json | grep -F 'v1.36.2'
container=$(docker run -d --name "rsdw-c2-release-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}" \
  -e RSDW_ADMIN_TOKEN=ci-smoke-token -p 127.0.0.1::8080 "$digest")
address=$(docker port "$container" 8080/tcp)
curl --fail --silent --show-error --retry 15 --retry-connrefused --retry-delay 1 \
  "http://$address/api/auth" | grep -F '"required":true'

printf '%s' "$GH_TOKEN" | helm registry login ghcr.io --username "$GITHUB_ACTOR" --password-stdin
mkdir "$task_dir/pulled" "$task_dir/expected" "$task_dir/actual"
if helm pull "$chart" --version "$version" --destination "$task_dir/pulled" 2>"$task_dir/chart.err"; then
  printf 'Reusing existing chart %s\n' "$version"
else
  cat "$task_dir/chart.err" >&2
  grep -Fq "ghcr.io/petzkod5/charts/rsdw-c2:$version: not found" "$task_dir/chart.err"
  helm push "$archive" oci://ghcr.io/petzkod5/charts
  helm pull "$chart" --version "$version" --destination "$task_dir/pulled"
fi
tar -xzf "$archive" -C "$task_dir/expected"
tar -xzf "$task_dir/pulled/rsdw-c2-$version.tgz" -C "$task_dir/actual"
diff -ru "$task_dir/expected" "$task_dir/actual"
node .github/check-chart.mjs "$task_dir/pulled/rsdw-c2-$version.tgz" "$version" "${CHART_CHECK_MODE:---kind}"
printf 'Published and verified image %s and chart %s:%s\n' "$digest" "$chart" "$version"
