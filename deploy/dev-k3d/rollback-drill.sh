#!/usr/bin/env bash
# Rollout and rollback drill on the disposable k3d cluster (setup.sh first).
# Uses its own namespace (tm-rollback) and database (tm_rollback). Images:
#   team-memory:0.5.0         previous release (stage 05, commit dca9066)
#   team-memory:0.6.0-dev     this release (adds migration 00007)
#   team-memory:0.6.1-badmig  this release plus a migration that fails
# A probe sends one MCP request through the ingress every 100 ms for the
# whole drill and records the status of each; the summary is printed at the end.
set -euo pipefail
cd "$(dirname "$0")/../.."
export KUBECONFIG=$PWD/deploy/dev-k3d/.local/kubeconfig
L=deploy/dev-k3d/.local; source $L/env; CA=$L/ca.pem
NS=tm-rollback REL=tm-rb FULL=tm-rb-team-memory HOST=rb.localtest.me
CHART=deploy/helm/team-memory A=$CHART/scripts/tm-admin.sh
OUT=${DRILL_OUT:-$L/rollback-drill}; mkdir -p "$OUT"
psql() { kubectl -n tm-db exec postgres-0 -- psql -U teammemory "$@"; }
step() { echo "== $(date +%T) $*"; }

step "fresh database and namespace"
psql -qc "DROP DATABASE IF EXISTS tm_rollback WITH (FORCE)" -c "CREATE DATABASE tm_rollback" >/dev/null
for r in tm_rb_app; do psql -qc "DROP ROLE IF EXISTS $r" >/dev/null; done
kubectl create namespace $NS --dry-run=client -o yaml | kubectl apply -f - >/dev/null
kubectl -n $NS create secret tls localtest-tls --cert=$L/tls.pem --key=$L/tls-key.pem --dry-run=client -o yaml | kubectl apply -f - >/dev/null
DB=postgres.tm-db.svc:5432/tm_rollback?sslmode=disable
kubectl -n $NS create secret generic tm-rb-db --from-literal=operator-url="postgres://teammemory:$PG_PASSWORD@$DB" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
export TM_DB_SECRET=tm-rb-db TM_INSTANCE=$REL
TM_IMAGE=team-memory:0.5.0 $A $NS $FULL migrate
APPPW=$(TM_IMAGE=team-memory:0.5.0 $A $NS $FULL db create-login --role app --user tm_rb_app | grep -E '^[A-Za-z0-9_-]{32}$')
kubectl -n $NS patch secret tm-rb-db --type merge -p "{\"stringData\":{\"app-url\":\"postgres://tm_rb_app:$APPPW@$DB\"}}" >/dev/null
cat > $OUT/values.yaml <<YAML
publicURL: https://$HOST
image: {repository: team-memory, pullPolicy: Never}
database: {existingSecret: tm-rb-db}
ingress: {host: $HOST, tls: {secretName: localtest-tls}}
trustedProxies: [10.42.0.0/16]
api: {shutdownDelay: 5s}
networkPolicy:
  database: {to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: tm-db}}}]}
YAML
up() { helm upgrade --install $REL $CHART -n $NS -f $OUT/values.yaml --set image.tag=$1 --wait --timeout 4m "${@:2}"; }

step "install the previous release (0.5.0)"
up 0.5.0 >/dev/null
TOKEN=$(TM_IMAGE=team-memory:0.5.0 $A $NS $FULL init --org drill --owner "Drill Owner" | grep -o 'tmpat_[A-Za-z0-9_-]*')
mcp() { curl -s --cacert $CA --max-time 3 -o /dev/null -w '%{http_code}' https://$HOST/mcp -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' -H 'MCP-Protocol-Version: 2025-11-25' -d "$1"; }
write() { mcp '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"memory_create","arguments":{"scope":"drill/default","kind":"fact","title":"'"$1"'","content":"written during the rollback drill"}}}'; }
schema() { psql -d tm_rollback -tAc "SELECT max(version_id) FROM goose_db_version WHERE is_applied"; }
echo "write on 0.5.0: $(write 'on 0.5.0'); schema $(schema)"

step "probe starts"
: > $OUT/probe.log
( while :; do printf '%s %s\n' "$(perl -MTime::HiRes=time -e 'printf "%d", time*1000')" "$(mcp '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"memory_search","arguments":{"scope":"drill/default","query":"drill"}}}')"; sleep 0.1; done ) >> $OUT/probe.log &
PROBE=$!; trap 'kill $PROBE 2>/dev/null || true' EXIT
mark() { echo "$(perl -MTime::HiRes=time -e 'printf "%d", time*1000') MARK $*" >> $OUT/probe.log; step "$*"; }
sleep 5

mark "upgrade 0.5.0 -> 0.6.0-dev (migration 00007)"
up 0.6.0-dev >/dev/null; echo "write on 0.6.0: $(write 'on 0.6.0'); schema $(schema)"
sleep 5
mark "rollback to revision 1 (0.5.0 binary on schema 7)"
helm rollback $REL 1 -n $NS --wait --timeout 4m >/dev/null
kubectl -n $NS get deploy $FULL-api -o jsonpath='image now: {.spec.template.spec.containers[0].image}{"\n"}'
echo "write on rolled-back 0.5.0: $(write 'after rollback'); schema $(schema)"
sleep 5
mark "roll forward again to 0.6.0-dev"
up 0.6.0-dev >/dev/null; echo "write: $(write 'forward again'); schema $(schema)"
sleep 5
mark "upgrade to 0.6.1-badmig (failing migration)"
if up 0.6.1-badmig > $OUT/badmig.out 2>&1; then echo "UNEXPECTED: bad upgrade succeeded"; else echo "upgrade failed as expected: $(grep -o 'pre-upgrade hooks failed[^"]*' $OUT/badmig.out | head -1)"; fi
kubectl -n $NS get deploy $FULL-api -o jsonpath='image still: {.spec.template.spec.containers[0].image}{"\n"}'
echo "schema $(schema); column rollback_drill exists: $(psql -d tm_rollback -tAc "SELECT count(*) FROM information_schema.columns WHERE table_name='memories' AND column_name='rollback_drill'")"
echo "write after failed upgrade: $(write 'after bad migration')"
helm history $REL -n $NS | tail -6
sleep 5
mark "end"
kill $PROBE; trap - EXIT

python3 - $OUT/probe.log <<'PY'
import sys, collections
seg, counts, fails = "before upgrade", collections.OrderedDict(), []
for line in open(sys.argv[1]):
    t, rest = line.split(" ", 1)
    if rest.startswith("MARK"):
        seg = rest[5:].strip(); continue
    c = counts.setdefault(seg, collections.Counter()); c[rest.strip()] += 1
    if rest.strip() != "200": fails.append((int(t), seg, rest.strip()))
for seg, c in counts.items():
    print(f"{seg:55s} {dict(c)}")
print("failed probes:", len(fails), fails[:10])
PY
