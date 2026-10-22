# Restore drill report

## 1. PostgreSQL: WAL archiving and base backup (2026-10-22)

Script: `deploy/backup/drill.sh` (`make backup-drill`). Throwaway containers and volumes; the repository is a separate volume that survives the "disaster".

| | |
| --- | --- |
| Host | Apple M4, Docker Desktop 29.9.2 |
| PostgreSQL | 17.8 (Debian, PGDG) + pgvector 0.8.1 |
| pgBackRest | 2.59.3, POSIX repository, `zst` compression |
| Archiving | `archive_mode = on`, `archive_timeout = 60` (deploy/backup/postgresql-archiving.conf) |

### Procedure

1. Start the primary with WAL archiving; `stanza-create`; `pgbackrest check` (proves `archive_command` works).
2. Seed real Team Memory data through the service (`loadtest seed`): 341 scopes, 8,530 memories with embeddings, 301 principals, 1,200 grants.
3. **Full backup** (10 s; 140 MB database).
4. More Team Memory writes **after** the backup (another organization: 31 scopes, memories, 51 principals). These exist only in WAL.
5. With writes quiet, checksum every Team Memory table: row count and an MD5 over all rows, for 20 tables (scopes, closure, settings, members, access, principals, identities, tokens, memories, revisions, overrides, events, approvals, promotion requests, embeddings, conflict candidates, jobs, mirror state, audit, migration versions).
6. Marker writer: one committed row per second; the time of each commit is recorded on the client.
7. After 150 markers, `docker kill -s KILL` the primary. Its data volume is treated as lost from here.
8. Restore into a **new, empty** volume from the repository only (`pgbackrest restore --type=default`, which replays every archived WAL segment), start PostgreSQL, wait until it leaves recovery.
9. Verify: checksums of the 20 tables, markers recovered, `amcheck` on every btree index, and `team-memory scope list` with the real binary against the restored database.

### Results

| Measure | Result | Target |
| --- | --- | --- |
| Data integrity | **identical** checksums for all 20 Team Memory tables, including the data written after the base backup (replayed from WAL) | |
| Index consistency | 73 btree indexes pass `bt_index_check` | |
| Application check | `team-memory scope list` works: 372 scopes | |
| Markers | 150 committed, 132 recovered, **18 lost** | |
| **Recovery point** | the last recovered marker was committed **24 s** before the primary was killed | 15 min |
| **Recovery time** | **12 s** from the kill to a writable restored instance; **24 s** including verification | 2 h |

The lost markers were in the WAL segment that was still being written when the server was killed. `archive_timeout = 60` bounds that window to about a minute (plus archiving lag) under any write load; 24 s is one sample inside it. The RPO target of 15 minutes is met with a wide margin **provided archiving is healthy**: the `TeamMemoryWALArchivingFailing` and `TeamMemoryWALArchiveStale` alerts guard that.

### What this drill does not show

- **Size**: 120–140 MB restores in seconds. Restore time grows with database size and WAL volume since the last backup; at the expected pilot size it remains far inside 2 hours, but a large database needs its own measurement. Most of a real RTO is decision time and provisioning, which this drill does not include.
- **Off-host repository and encryption**: production should use S3 (or similar) in another failure domain with `repo1-cipher-type`; not drilled here.
- **Point-in-time restore to an earlier time** (`--type=time`): documented in [backup-restore.md](backup-restore.md), not drilled.
- **The Kubernetes side**: re-pointing the release at the restored database is documented, not drilled here (the rollback drill exercised migrations and rollouts).

## 2. Git mirror rebuild (disaster-recovery exception), by hand (2026-10-22)

Following [mirror.md](mirror.md#disaster-recovery-rebuild-from-the-mirror) literally on the disposable k3d cluster. Source: the mirror repository of `acme/default` in Gitea over SSH (2 memories: an approved decision with a source URI, and an unverified fact).

1. Created the replacement scope `acme/default-restored` with `tm-admin.sh ... scope create`.
2. `team-memory mirror rebuild --repo <old scope id> --scope acme/default-restored --as <owner> --dry-run` listed both files as "skipped (dry run)".
3. The real run created both memories with new ids.
4. Check in the database: the approved decision came back as **`cited`, not approved**, and the fact as `unverified`. The change notes record the original id, revision and trust, plus "was approved by Ada Lovelace at revision 1 (approval not restored; review again)".

The integration test `TestMirrorDisasterRecoveryRebuild` covers superseded (skipped) and archived (`--include-archived`) memories as well.
