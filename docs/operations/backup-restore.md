# Backup and restore

PostgreSQL is the source of truth for everything: memories, revisions, events, approvals, audit, principals, tokens, OAuth state and embeddings. Back it up with **continuous WAL archiving plus base backups**. The git mirror is supplementary (content only) and never replaces this.

Targets (proposal): **RPO 15 minutes, RTO 2 hours.** Measured in the drill: see [restore-drill-report.md](restore-drill-report.md).

## Tool choice: pgBackRest

| Option | Why (not) |
| --- | --- |
| **pgBackRest** (chosen for external PostgreSQL) | Mature, open source (MIT), parallel and compressed full/diff/incremental backups, async WAL archiving, point-in-time restore, encryption, S3/GCS/Azure/POSIX repositories, `check`/`verify` commands, retention management. Runs on the database host, so no Team Memory pod needs backup access. |
| WAL-G | Similar scope, lighter; fewer verification and retention features. A fine alternative if you already run it. |
| `pg_dump` only | Logical snapshots cannot meet a 15-minute RPO without dumping every few minutes; no PITR. Useful **in addition** for portable exports. |
| Managed-service PITR | Use it when the database is a managed service; then this runbook's restore drill still applies, with the provider's restore. |
| CloudNativePG (in-cluster) | Its Barman Cloud integration provides WAL archiving and PITR to object storage; follow its documentation and still run the drill below. |

## Configuration (database host)

`deploy/backup/` has the reference files used by the drill:

- `postgresql-archiving.conf`: `wal_level = replica`, `archive_mode = on`, `archive_command = 'pgbackrest --stanza=tm archive-push %p'`, **`archive_timeout = 60`** (forces a WAL segment at least every minute when there is any write, so unarchived data is at most about a minute old).
- `pgbackrest.conf`: stanza `tm`, compression `zst`, `repo1-retention-full=2`, `repo1-retention-diff=7`.
- `Dockerfile`: pgvector 0.8.1 on PostgreSQL 17 with pgBackRest 2.59, for containerized hosts.

Production differences from the drill:

- Put the repository **off the database host and in another failure domain**: `repo1-type=s3` with a bucket in another region or provider, credentials from the host's secret store.
- Encrypt it: `repo1-cipher-type=aes-256-cbc` with `repo1-cipher-pass` from a secret store (never in git). Losing the passphrase loses every backup; store it twice.
- Consider a second repository (`repo2-*`) with longer retention.

Initialize once, then check:

```sh
sudo -u postgres pgbackrest --stanza=tm stanza-create
sudo -u postgres pgbackrest --stanza=tm check      # proves archive_command works
```

## Schedule

| What | When | Command |
| --- | --- | --- |
| WAL archiving | continuous; a segment at least every 60 s | `archive_command` |
| Full backup | weekly, Sunday 01:30 | `pgbackrest --stanza=tm --type=full backup` |
| Differential backup | daily, 01:30 other days | `pgbackrest --stanza=tm --type=diff backup` |
| Verify repository | weekly, after the full | `pgbackrest --stanza=tm verify` |
| Restore drill | quarterly, and after major upgrades | this page, "Restore" |

Expiry runs with each backup per the retention settings. Alerts: `TeamMemoryWALArchivingFailing`, `TeamMemoryWALArchiveStale` and disk alerts (`deploy/helm/team-memory/files/prometheus-rules.yaml`; they need postgres_exporter). Also alert when the newest backup is older than 26 hours (from `pgbackrest info --output=json`).

## Restore

Decide first: **full restore** (database lost) or **point-in-time restore into a scratch database** (bad migration, mistaken purge, need a few rows back).

### Full restore after losing the database

1. Stop writers: `kubectl -n team-memory scale deploy --all --replicas=0` (the API then serves nothing, which is honest; readiness would fail anyway).
2. On a new or repaired host with PostgreSQL 17 + pgvector 0.8 + pgBackRest and access to the repository, with an **empty** data directory:

   ```sh
   sudo -u postgres pgbackrest --stanza=tm --type=default restore     # replays all archived WAL
   # or to a point in time:
   sudo -u postgres pgbackrest --stanza=tm --type=time --target='2026-10-22 14:05:00+00' --target-action=promote restore
   systemctl start postgresql
   ```

3. Wait until `SELECT pg_is_in_recovery()` is false.
4. Verify: row counts of `memories`, `memory_revisions`, `memory_events`, `audit_events` look right; `team-memory scope list` works with the operator URL; `CREATE EXTENSION IF NOT EXISTS amcheck` and `bt_index_check` on the indexes.
5. If the host or address changed, update the URLs in the `team-memory-db` Secret. Login roles and their passwords are part of the restored cluster.
6. Run `pgbackrest --stanza=tm check` and take a **new full backup** (the restored timeline starts a new history).
7. Scale back up (`helm upgrade` with the same values, or scale each Deployment), `helm test`, then `mirror verify`. Mirror events that were pushed after the recovery point exist in git but not in the database: `verify` reports them as `unexpected_file` or `content_differs`. The next exports overwrite them; record the gap.
8. Tell users which time window was lost (the recovery point).

### Point-in-time restore into a scratch database

Restore to another data directory or host (`--pg1-path=/srv/scratch`, a different port, `--archive-mode=off` so the copy never archives into the production repository), copy out what you need with `pg_dump --table` or SQL, then **delete the scratch copy**. Restored copies are data copies: they fall under [secret-purge.md](secret-purge.md) step 4.

## The disaster-recovery fallback

If the database **and** every backup are lost, [mirror.md](mirror.md#disaster-recovery-rebuild-from-the-mirror) rebuilds memory content (not approvals, history, audit or memberships) from the git mirror. It is a last resort, not part of the RPO/RTO plan.
