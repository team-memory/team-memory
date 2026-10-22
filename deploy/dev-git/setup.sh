#!/usr/bin/env bash
# Starts the disposable Gitea and provisions what the authenticated mirror
# tests need, all under .local/ (git-ignored, throwaway):
#   certs/ca.pem          self-signed certificate for https://localhost:3443
#   token                 an access token with repository write scope
#   deploy_key(.pub)      an SSH deploy key with write access to one repo
#   known_hosts           Gitea's SSH host key for [localhost]:2222
#   env                   TEAM_MEMORY_MIRROR_TEST_* settings for go test
# Run `docker compose -f deploy/dev-git/compose.yaml down -v` to throw it away.
set -euo pipefail
cd "$(dirname "$0")"
here=$(pwd)
mkdir -p .local/certs
chmod 700 .local
umask 077

if [[ ! -f .local/certs/cert.pem ]]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
    -subj "/CN=localhost" -addext "subjectAltName=DNS:localhost,IP:127.0.0.1" \
    -keyout .local/certs/key.pem -out .local/certs/cert.pem 2>/dev/null
  chmod 644 .local/certs/*.pem   # read by the container's non-root user
  cp .local/certs/cert.pem .local/certs/ca.pem
fi

docker compose up -d --wait

gt() { docker compose exec -T gitea gitea "$@"; }
if [[ ! -f .local/admin-password ]]; then
  openssl rand -base64 24 | tr -dc 'A-Za-z0-9' > .local/admin-password
  gt admin user create --admin --username mirror-admin --email mirror-admin@example.com \
    --password "$(cat .local/admin-password)" --must-change-password=false >/dev/null
fi
pw=$(cat .local/admin-password)
api() { curl -fsS --cacert .local/certs/ca.pem -u "mirror-admin:$pw" -H 'Content-Type: application/json' "$@"; }

if [[ ! -f .local/token ]]; then
  api -X POST https://localhost:3443/api/v1/users/mirror-admin/tokens \
    -d "{\"name\":\"mirror-test-$(date +%s)\",\"scopes\":[\"write:repository\"]}" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["sha1"], end="")' > .local/token
fi

# Two repositories: https-repo (token) and ssh-repo (deploy key only).
for repo in https-repo ssh-repo; do
  api https://localhost:3443/api/v1/repos/mirror-admin/$repo >/dev/null 2>&1 ||
    api -X POST https://localhost:3443/api/v1/user/repos -d "{\"name\":\"$repo\",\"private\":true}" >/dev/null
done

if [[ ! -f .local/deploy_key ]]; then
  ssh-keygen -q -t ed25519 -N '' -C team-memory-mirror-test -f .local/deploy_key
  api -X POST https://localhost:3443/api/v1/repos/mirror-admin/ssh-repo/keys \
    -d "{\"title\":\"team-memory mirror\",\"key\":\"$(cat .local/deploy_key.pub)\",\"read_only\":false}" >/dev/null
fi
# A second key that is not registered anywhere, for the negative test.
[[ -f .local/other_key ]] || ssh-keygen -q -t ed25519 -N '' -C unregistered -f .local/other_key

# Pin the host key the way an operator would (from the server itself).
hostkey=$(docker compose exec -T gitea sh -c 'cat /var/lib/gitea/ssh/gitea.rsa.pub')
echo "[localhost]:2222 $(echo "$hostkey" | awk '{print $1" "$2}')" > .local/known_hosts

cat > .local/env <<ENV
TEAM_MEMORY_MIRROR_TEST_HTTPS=https://localhost:3443/mirror-admin/{repo}.git
TEAM_MEMORY_MIRROR_TEST_SSH=ssh://git@localhost:2222/mirror-admin/{repo}.git
TEAM_MEMORY_MIRROR_TEST_TOKEN_FILE=$here/.local/token
TEAM_MEMORY_MIRROR_TEST_TOKEN_USER=mirror-admin
TEAM_MEMORY_MIRROR_TEST_CA_FILE=$here/.local/certs/ca.pem
TEAM_MEMORY_MIRROR_TEST_SSH_KEY_FILE=$here/.local/deploy_key
TEAM_MEMORY_MIRROR_TEST_OTHER_KEY_FILE=$here/.local/other_key
TEAM_MEMORY_MIRROR_TEST_KNOWN_HOSTS=$here/.local/known_hosts
ENV
echo "dev git server ready; settings in $here/.local/env"
