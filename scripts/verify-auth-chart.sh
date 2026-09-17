#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
oidcBase=(--set auth.mode=oidc --set auth.oidc.issuer=https://issuer.example/oidc --set auth.oidc.publicOrigin=https://console.example --set auth.oidc.clientID=console --set auth.oidc.clientSecret.name=console-oidc)
oidc=("${oidcBase[@]}" --set 'auth.oidc.rolePolicy.viewerGroups[0]=readers')
rendered=$(helm template auth-test charts/rsdw-c2 "${oidc[@]}")
[[ "$rendered" == *'RSDW_OIDC_CLIENT_SECRET'* && "$rendered" == *'name: "console-oidc"'* && "$rendered" != *'RSDW_ADMIN_TOKEN'* ]]
helm lint charts/rsdw-c2 "${oidc[@]}"
rendered=$(helm template auth-test charts/rsdw-c2 -f charts/rsdw-c2/values-pocketid.yaml)
[[ "$rendered" == *'https://auth.petzko.sh'* && "$rendered" == *'openid email profile groups'* && "$rendered" == *'rsdw_c2_admins'* && "$rendered" == *'rsdw_c2_users'* && "$rendered" != *'RSDW_ADMIN_TOKEN'* ]]
helm template auth-test charts/rsdw-c2 "${oidcBase[@]}" --set 'auth.oidc.rolePolicy.adminSubjects[0]=operator-subject' >/dev/null
rendered=$(helm template auth-test charts/rsdw-c2 --set auth.adminTokenSecret.name=console-admin)
[[ "$rendered" == *'RSDW_ADMIN_TOKEN'* && "$rendered" != *'RSDW_OIDC_CLIENT_SECRET'* ]]

reject() {
  if helm template auth-test charts/rsdw-c2 "$@" >/dev/null 2>&1; then
    printf 'Unexpected chart success: %s\n' "$*" >&2
    exit 1
  fi
}
reject
reject --set auth.mode=unknown
reject "${oidc[@]}" --set auth.adminTokenSecret.name=mixed
reject "${oidc[@]}" --set auth.mode=token --set auth.adminTokenSecret.name=mixed
reject "${oidc[@]}" --set auth.oidc.issuer=http://issuer.example
reject "${oidc[@]}" --set auth.oidc.issuer='https://issuer.example?tenant=one'
reject "${oidc[@]}" --set auth.oidc.publicOrigin=http://console.example
reject "${oidc[@]}" --set auth.oidc.publicOrigin=https://console.example/path
reject "${oidc[@]}" --set auth.oidc.publicOrigin='https://console.example?x=1'
reject "${oidc[@]}" --set auth.oidc.clientID=
reject "${oidc[@]}" --set auth.oidc.clientSecret.name=
reject "${oidc[@]}" --set auth.oidc.clientSecret.key=
reject "${oidcBase[@]}"
reject "${oidc[@]}" --set 'auth.oidc.rolePolicy.viewerGroups[0]= '
reject "${oidc[@]}" --set-json 'auth.oidc.scopes=["profile"]'
reject "${oidc[@]}" --set-json 'auth.oidc.scopes=["openid","offline_access"]'
reject "${oidc[@]}" --set auth.oidc.insecure=true
reject "${oidc[@]}" --set replicaCount=2
printf '%s\n' 'Auth chart checks passed'
