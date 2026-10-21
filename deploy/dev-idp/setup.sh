#!/usr/bin/env bash
# Generates throwaway credentials for the local dev IdPs and a Team Memory
# SSO configuration that uses them. Everything goes to .local/ (git-ignored).
# Rerunning keeps existing values; delete .local/ to start over.
set -euo pipefail
cd "$(dirname "$0")"
here=$(pwd)
mkdir -p .local/keys .local/secrets
chmod 700 .local .local/keys .local/secrets

gen() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-32; }

if [[ ! -f .local/env ]]; then
  bob_password=$(gen)
  # Dex wants a bcrypt hash; htpasswd emits $2y$, the same algorithm as $2a$.
  bob_hash=$(htpasswd -bnBC 10 "" "$bob_password" | tr -d ':\n' | sed 's/^\$2y\$/$2a$/')
  umask 077
  cat > .local/env <<EOF
KC_BOOTSTRAP_ADMIN_PASSWORD=$(gen)
KC_CLIENT_SECRET=$(gen)
DEX_CLIENT_SECRET=$(gen)
ALICE_PASSWORD=$(gen)
EVE_PASSWORD=$(gen)
BOB_PASSWORD=$bob_password
BOB_PASSWORD_HASH='$bob_hash'
EOF
fi
# shellcheck disable=SC1091
source .local/env

umask 077
printf '%s' "$KC_CLIENT_SECRET" > .local/secrets/keycloak-client-secret
printf '%s' "$DEX_CLIENT_SECRET" > .local/secrets/dex-client-secret
[[ -f .local/keys/dev-1.pem ]] || openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out .local/keys/dev-1.pem 2>/dev/null

cat > .local/sso.json <<EOF
{
  "issuer": "http://localhost:8080",
  "allow_insecure_http": true,
  "signing_keys_dir": "$here/.local/keys",
  "access_token_ttl": "15m",
  "refresh_token_ttl": "720h",
  "client_registration": {"client_id_metadata_documents": true, "dynamic": true},
  "providers": [
    {
      "id": "keycloak",
      "display_name": "Keycloak (dev)",
      "issuer": "http://localhost:8180/realms/team-memory-dev",
      "client_id": "team-memory",
      "client_secret_file": "$here/.local/secrets/keycloak-client-secret",
      "allowed_email_domains": ["example.com"]
    },
    {
      "id": "dex",
      "display_name": "Dex (dev, no DCR)",
      "issuer": "http://localhost:5556/dex",
      "client_id": "team-memory",
      "client_secret_file": "$here/.local/secrets/dex-client-secret",
      "allowed_email_domains": ["example.com"]
    }
  ]
}
EOF

cat <<EOF
Dev IdP credentials are in $here/.local/env (throwaway, local only):
  Keycloak users: alice@example.com (allowed), eve@evil.test (refused by the domain rule)
  Dex user:       bob@example.com
  Keycloak admin: admin (http://localhost:8180)
Team Memory SSO config: $here/.local/sso.json

Next:
  docker compose -f $here/compose.yaml up -d --wait
  TEAM_MEMORY_SSO_CONFIG=$here/.local/sso.json TEAM_MEMORY_PUBLIC_URL=http://localhost:8080 ./bin/team-memory serve
EOF
