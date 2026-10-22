#!/usr/bin/env bash
# Provisions the identities and scopes for the stage 07 cross-client test on
# the disposable k3d cluster (deploy/dev-k3d/setup.sh). Idempotent.
#
#   Keycloak users (disposable realm)   role
#   wendy@example.com                   writer on the project
#   bob@example.com                     reader on the project
#   carol@example.com                   maintainer of org, team and project
#   dave@example.com                    signs in, holds no membership
#
# Scopes: acme (org) > acme/platform (team) > acme/platform/payments (project).
# (alice is the stage 06 owner's linked SSO identity, so it is not used as a plain writer.)
# Passwords are generated and written to .local/env (git-ignored), never printed.
set -euo pipefail
cd "$(dirname "$0")"
root=$(cd ../.. && pwd)
source .local/env
CA=$PWD/.local/ca.pem
ADMIN="$root/deploy/helm/team-memory/scripts/tm-admin.sh"
ctx=k3d-tm-dev
export KUBECONFIG=${KUBECONFIG:-$PWD/.local/kubeconfig}
kc=https://keycloak.localtest.me
realm=team-memory-dev
issuer=$kc/realms/$realm

tok=$(curl -s --cacert "$CA" -d client_id=admin-cli -d grant_type=password -d username=admin \
  --data-urlencode "password=$KC_BOOTSTRAP_ADMIN_PASSWORD" "$kc/realms/master/protocol/openid-connect/token" | jq -r .access_token)
[[ $tok != null && -n $tok ]] || { echo "Keycloak admin login failed (username 'admin' assumed)"; exit 1; }
api() { curl -s --cacert "$CA" -H "Authorization: Bearer $tok" -H 'Content-Type: application/json' "$@"; }

declare -A sub
for u in wendy bob carol dave; do
  var=$(echo "${u^^}_PASSWORD")
  if [[ -z ${!var:-} ]]; then
    pw=$(openssl rand -hex 14)
    echo "$var=$pw" >> .local/env
    eval "$var=$pw"
  fi
  id=$(api "$kc/admin/realms/$realm/users?username=$u&exact=true" | jq -r '.[0].id // empty')
  if [[ -z $id ]]; then
    api -X POST "$kc/admin/realms/$realm/users" -d "$(jq -cn --arg u "$u" --arg p "${!var}" \
      '{username:$u,email:($u+"@example.com"),emailVerified:true,enabled:true,firstName:($u),lastName:"Example",credentials:[{type:"password",value:$p,temporary:false}]}')" >/dev/null
    id=$(api "$kc/admin/realms/$realm/users?username=$u&exact=true" | jq -r '.[0].id')
  fi
  sub[$u]=$id
done

tm() { "$ADMIN" team-memory team-memory "$@"; }
tm scope create --parent acme --level team --slug platform 2>&1 | tail -1 || true
tm scope create --parent acme/platform --level project --slug payments 2>&1 | tail -1 || true
for u in wendy bob carol dave; do
  tm principal create --name "${u^}" 2>&1 | tail -2
done
echo "Next: link each principal (principal link-identity --issuer $issuer --subject <sub>) and add members."
declare -p sub
