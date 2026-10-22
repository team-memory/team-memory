#!/usr/bin/env bash
# Restore drill: proves that WAL archiving plus base backups restore Team
# Memory's database into a scratch instance with intact data, and measures
# the recovery point (data lost) and recovery time. Throwaway containers
# and volumes only (names tm-drill-*). Results go to .local/drill-*.txt.
#
#   deploy/backup/drill.sh            # needs Docker, Go, and Ollama at 127.0.0.1:11434 (optional)
set -euo pipefail
cd "$(dirname "$0")"; here=$(pwd); root=$(cd ../.. && pwd)
mkdir -p .local; umask 077
PW=$(openssl rand -hex 16)
IMG=team-memory-pg-backrest:drill
NET=tm-drill-net
log() { echo "== $(date +%T) $*"; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d", time*1000'; }
cleanup() { docker rm -f tm-drill-primary tm-drill-restore >/dev/null 2>&1 || true; docker volume rm tm-drill-pgdata tm-drill-repo tm-drill-restore >/dev/null 2>&1 || true; docker network rm $NET >/dev/null 2>&1 || true; }
cleanup
docker build -q -t $IMG . >/dev/null
docker network create $NET >/dev/null
psqlp() { docker exec -u postgres tm-drill-primary psql -X -d teammemory -v ON_ERROR_STOP=1 "$@"; }
psqlr() { docker exec -u postgres tm-drill-restore psql -X -d teammemory -v ON_ERROR_STOP=1 "$@"; }

log "primary with WAL archiving"
# The settings of postgresql-archiving.conf, as -c name=value arguments.
ARGS=()
while IFS= read -r line; do
  line=${line%%#*}; [[ $line =~ ^[[:space:]]*([a-z_]+)[[:space:]]*=[[:space:]]*(.*[^[:space:]])[[:space:]]*$ ]] || continue
  v=${BASH_REMATCH[2]}; v=${v#\'}; v=${v%\'}
  ARGS+=(-c "${BASH_REMATCH[1]}=$v")
done < postgresql-archiving.conf
docker run -d --name tm-drill-primary --network $NET -p 127.0.0.1:55433:5432 \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=$PW -e POSTGRES_DB=teammemory -e PGDATA=/var/lib/postgresql/data/pgdata \
  -v tm-drill-pgdata:/var/lib/postgresql/data -v tm-drill-repo:/var/lib/pgbackrest \
  $IMG postgres "${ARGS[@]}" >/dev/null
wait_ready() { # container: fails fast if it stops
  for _ in $(seq 1 600); do
    [[ $(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null) == true ]] || { docker logs "$1" 2>&1 | tail -5; echo "$1 stopped"; exit 1; }
    docker exec "$1" pg_isready -U postgres -d teammemory >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  echo "$1 not ready after 5 minutes"; exit 1
}
wait_ready tm-drill-primary; sleep 3; wait_ready tm-drill-primary   # the entrypoint restarts the server once after init
docker exec -u postgres tm-drill-primary psql -X -d teammemory -tAc "SHOW archive_command" | grep -q pgbackrest || { echo "archiving not configured"; exit 1; }
docker exec tm-drill-primary chown postgres:postgres /var/lib/pgbackrest
docker exec -u postgres tm-drill-primary pgbackrest --stanza=tm stanza-create >/dev/null
docker exec -u postgres tm-drill-primary pgbackrest --stanza=tm check >/dev/null && log "archiving works (pgbackrest check)"

URL="postgres://postgres:$PW@127.0.0.1:55433/teammemory?sslmode=disable"
log "seed Team Memory data (before the base backup)"
(cd "$root" && DATABASE_URL=$URL go run ./loadtest seed --users "$here/.local/drill-users.json" --org drill --branching 4 --memories-per-project 30 --principals 300 --load-users 4 | tail -2)
(cd "$root" && DATABASE_URL=$URL go run ./loadtest embed-synthetic 2>/dev/null | tail -1) || echo "no Ollama: no embeddings in the drill data"

log "full backup"
t0=$(now_ms); docker exec -u postgres tm-drill-primary pgbackrest --stanza=tm --type=full backup >/dev/null; t1=$(now_ms)
FULL_S=$(( (t1-t0)/1000 ))
log "more Team Memory writes after the backup (present only in WAL)"
(cd "$root" && DATABASE_URL=$URL go run ./loadtest seed --users "$here/.local/drill-users2.json" --org after --branching 2 --memories-per-project 40 --principals 50 --load-users 1 | tail -1)

TABLES="scopes scope_closure org_settings scope_members scope_access principals principal_identities personal_access_tokens memories memory_revisions revision_overrides memory_events approvals promotion_requests memory_embeddings conflict_candidates jobs mirror_state audit_events goose_db_version"
checksum() { for t in $TABLES; do echo "$t $($1 -tAc "SELECT count(*) || ' ' || coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)), '-') FROM $t x")"; done; }
log "checksums of every Team Memory table (writes are quiet now)"
checksum psqlp > .local/drill-before.txt
psqlp -qc "CREATE TABLE drill_markers (seq int PRIMARY KEY, at timestamptz NOT NULL DEFAULT clock_timestamp())"

log "marker writer: one committed row per second for 150 s, then the primary is killed"
: > .local/drill-markers.log
for i in $(seq 1 150); do psqlp -qc "INSERT INTO drill_markers (seq) VALUES ($i)" && echo "$i $(now_ms)" >> .local/drill-markers.log; sleep 1; done
LAST=$(tail -1 .local/drill-markers.log | cut -d' ' -f1)
DB_SIZE=$(psqlp -tAc "SELECT pg_size_pretty(pg_database_size('teammemory'))")
docker kill -s KILL tm-drill-primary >/dev/null; T_DISASTER=$(now_ms)
log "primary killed (SIGKILL) after committing marker $LAST; the primary's volume is now treated as lost"
docker rm tm-drill-primary >/dev/null

log "restore into a scratch instance from the repository only"
docker volume create tm-drill-restore >/dev/null
docker run -d --name tm-drill-restore --network $NET -p 127.0.0.1:55434:5432 -e PGDATA=/var/lib/postgresql/data/pgdata \
  -v tm-drill-restore:/var/lib/postgresql/data -v tm-drill-repo:/var/lib/pgbackrest \
  --entrypoint bash $IMG -c "chown postgres:postgres /var/lib/postgresql/data && su postgres -c 'pgbackrest --stanza=tm --type=default --archive-mode=off restore' && exec su postgres -c 'postgres -c archive_mode=off'" >/dev/null
wait_ready tm-drill-restore
until [[ $(psqlr -tAc "SELECT pg_is_in_recovery()" 2>/dev/null) == f ]]; do sleep 0.5; done
T_READY=$(now_ms)
log "restored instance accepts writes"
checksum psqlr > .local/drill-after.txt
REC=$(psqlr -tAc "SELECT coalesce(max(seq), 0) FROM drill_markers")
if diff -q .local/drill-before.txt .local/drill-after.txt >/dev/null; then INTEGRITY=identical; else INTEGRITY=DIFFERENT; diff .local/drill-before.txt .local/drill-after.txt | head; fi
AMCHECK=$(psqlr -tAc "CREATE EXTENSION IF NOT EXISTS amcheck; SELECT count(*) FROM (SELECT bt_index_check(c.oid) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid JOIN pg_am a ON a.oid = c.relam WHERE a.amname = 'btree' AND c.relnamespace = 'public'::regnamespace) s" | tail -1)
# The restored database works for Team Memory itself, not only for psql.
TMCHECK=$(cd "$root" && DATABASE_URL="postgres://postgres:$PW@127.0.0.1:55434/teammemory?sslmode=disable" go run ./cmd/team-memory scope list | wc -l | tr -d ' ')
T_VERIFIED=$(now_ms)
LOST=$(( LAST - REC ))
if (( REC > 0 )); then REC_AT=$(awk -v s=$REC '$1==s{print $2}' .local/drill-markers.log); RPO_S=$(( (T_DISASTER - REC_AT) / 1000 )); else RPO_S=unknown; fi
INFO=$(docker exec -u postgres tm-drill-restore pgbackrest --stanza=tm info | grep -E "full backup|database size|repo1: backup size|wal archive min/max" | sed 's/^ *//' | tr '\n' ';')
cat > .local/drill-result.txt <<RES
date: $(date -u +%FT%TZ)
host: $(sysctl -n machdep.cpu.brand_string 2>/dev/null || uname -m), Docker $(docker version --format '{{.Server.Version}}')
postgres: $(psqlr -tAc "SHOW server_version"), pgbackrest: $(docker exec tm-drill-restore pgbackrest version)
database size at the disaster: $DB_SIZE
full backup duration: ${FULL_S}s
pgbackrest info: $INFO
team memory tables after restore vs before the marker phase: $INTEGRITY ($(wc -l < .local/drill-before.txt | tr -d ' ') tables)
btree indexes checked with amcheck: $AMCHECK
team-memory scope list on the restored database: $((TMCHECK - 1)) scopes
markers committed: $LAST, recovered: $REC, lost: $LOST
recovery point: last recovered marker committed ${RPO_S}s before the primary was killed
recovery time: $(( (T_READY - T_DISASTER) / 1000 ))s from kill to a writable restored instance, $(( (T_VERIFIED - T_DISASTER) / 1000 ))s including verification
RES
cat .local/drill-result.txt
[[ ${KEEP:-} ]] || cleanup
