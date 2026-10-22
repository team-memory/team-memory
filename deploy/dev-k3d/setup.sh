#!/usr/bin/env bash
# Creates the disposable k3d cluster "tm-dev" used to test the chart and
# docs/operations/authentication.md: Traefik on https://*.localtest.me
# (127.0.0.1:443) with a throwaway local CA, CoreDNS rewriting those names
# to Traefik inside the cluster, PostgreSQL 17 + pgvector in tm-db and
# Keycloak 26 in keycloak. It never touches any other kube context.
# Tear down: k3d cluster delete tm-dev
set -euo pipefail
cd "$(dirname "$0")"
here=$(pwd); root=$(cd ../.. && pwd)
mkdir -p .local && chmod 700 .local
umask 077
gen() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | cut -c1-28; }

# Throwaway CA and a wildcard certificate for *.localtest.me.
if [[ ! -f .local/ca.pem ]]; then
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj "/CN=tm-dev throwaway CA" \
    -keyout .local/ca-key.pem -out .local/ca.pem 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=*.localtest.me" \
    -keyout .local/tls-key.pem -out .local/tls.csr 2>/dev/null
  printf 'subjectAltName=DNS:*.localtest.me,DNS:localtest.me\nextendedKeyUsage=serverAuth\n' > .local/ext.cnf
  openssl x509 -req -in .local/tls.csr -CA .local/ca.pem -CAkey .local/ca-key.pem -CAcreateserial -days 30 \
    -extfile .local/ext.cnf -out .local/tls.pem 2>/dev/null
fi
if [[ ! -f .local/env ]]; then
  cat > .local/env <<ENV
PG_PASSWORD=$(gen)
KC_BOOTSTRAP_ADMIN_PASSWORD=$(gen)
KC_CLIENT_SECRET=$(gen)
ALICE_PASSWORD=$(gen)
EVE_PASSWORD=$(gen)
ENV
fi
# shellcheck disable=SC1091
source .local/env

if ! k3d cluster list tm-dev >/dev/null 2>&1; then
  k3d cluster create tm-dev --agents 1 -p "443:443@loadbalancer" --wait --kubeconfig-switch-context=false
fi
export KUBECONFIG="$here/.local/kubeconfig"
k3d kubeconfig get tm-dev > "$KUBECONFIG"
kubectl config current-context

# Inside the cluster, *.localtest.me must reach Traefik, not the pod's loopback.
kubectl apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata: {name: coredns-custom, namespace: kube-system}
data:
  localtest.override: |
    rewrite name regex (.*)\.localtest\.me traefik.kube-system.svc.cluster.local answer auto
YAML
kubectl -n kube-system rollout restart deploy/coredns >/dev/null

kubectl apply -f fixtures.yaml >/dev/null
for ns in tm-db keycloak team-memory; do
  kubectl create namespace $ns --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
kubectl -n tm-db create secret generic postgres-superuser --from-literal=password="$PG_PASSWORD" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
for ns in keycloak team-memory; do
  kubectl -n $ns create secret tls localtest-tls --cert=.local/tls.pem --key=.local/tls-key.pem --dry-run=client -o yaml | kubectl apply -f - >/dev/null
done
kubectl -n team-memory create configmap localtest-ca --from-file=ca.pem=.local/ca.pem --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# Keycloak realm: the dev realm with the cluster redirect URI.
python3 - "$root/deploy/dev-idp/keycloak-realm.json" > .local/realm.json <<'PY'
import json, sys
r = json.load(open(sys.argv[1]))
for c in r.get("clients", []):
    if c["clientId"] == "team-memory":
        c["redirectUris"] = ["https://memory.localtest.me/oauth/callback/keycloak"]
json.dump(r, sys.stdout)
PY
kubectl -n keycloak create configmap keycloak-realm --from-file=team-memory-dev.json=.local/realm.json --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n keycloak create secret generic keycloak-env --from-env-file=.local/env --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# The Team Memory image under test.
docker build -q --build-arg VERSION="${TM_VERSION:-0.6.0-dev}" -t "team-memory:${TM_VERSION:-0.6.0-dev}" "$root" >/dev/null
k3d image import -c tm-dev "team-memory:${TM_VERSION:-0.6.0-dev}" >/dev/null

kubectl -n tm-db rollout status statefulset/postgres --timeout=3m
kubectl -n keycloak rollout status deploy/keycloak --timeout=5m
echo "cluster ready. export KUBECONFIG=$KUBECONFIG"
echo "PostgreSQL superuser URL (cluster-internal): postgres://teammemory:<PG_PASSWORD in .local/env>@postgres.tm-db.svc:5432/teammemory?sslmode=disable"
