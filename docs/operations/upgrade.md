# Upgrade, staged rollout and rollback

## Migration policy: forward-only, backward compatible

- Migrations are **forward-only** in production. Each release's migrations must keep the **previous release** working (expand/contract): add tables, columns, policies and triggers; never drop or rename something the previous release reads in the same release. Removals happen one release later, after nobody can roll back past it.
- So a bad **release** is rolled back by rolling back the **pods** (`helm rollback`); the schema stays where it is. Down migrations exist for development only and are never run by the chart. Migrations that would destroy history (`00001`, `00003`) refuse to go down at all.
- goose runs each migration file in **one transaction**. A failing migration leaves no partial change, the pre-upgrade hook fails, Helm stops the upgrade before touching any Deployment, and the old release keeps serving.
- The migration hook runs before every install and upgrade, as the operator role; `helm rollback` does not run it.
- Reviewers of a migration check: does the previous binary still start (role checks), read and write against the new schema? Is the migration safe to run while the old binary serves traffic (no long exclusive locks on hot tables; build large indexes `CONCURRENTLY` in their own migration with `-- +goose NO TRANSACTION`)?

## Staged rollout

| Stage | Where | Gate to move on |
| --- | --- | --- |
| 1. Dev | `make test` (unit, integration, security suite), the browser test, `helm lint`/`helm template`, and the disposable k3d cluster (`deploy/dev-k3d/setup.sh`) | Everything green; `rollback-drill.sh` passes for releases with migrations |
| 2. Private cluster (pilot) | The author's MicroK8s cluster, one project, several machines | 1 week without unexplained alerts; `mirror verify` clean; a restore drill done on the real database in the last 90 days |
| 3. Wider use | More projects, then more people | Pilot report reviewed; stage 07 cross-client checks pass |

For each upgrade:

1. Read the release notes for migrations and new settings.
2. Check the backup is current: `pgbackrest --stanza=tm info` (last full or diff backup under 24 h old; WAL archiving not failing).
3. `helm diff upgrade` (plugin) or `helm template` and compare.
4. `helm upgrade team-memory deploy/helm/team-memory -n team-memory -f values.yaml --set image.digest=sha256:... --wait`
5. Watch: `kubectl rollout status`, `TeamMemoryNotReady`, error rate, `helm test`.
6. After the rollout: `mirror verify` if the mirror is on.

## Rollback

```sh
helm history team-memory -n team-memory
helm rollback team-memory <previous revision> -n team-memory --wait
```

This restores the previous image and values; the schema stays at the newer version, which the previous release supports by the policy above.

### A migration fails during upgrade

Nothing to roll back: the transaction rolled back, the hook Job failed, the Deployments were not changed, and Helm marks the revision `failed`. Read the Job's log (`kubectl -n team-memory logs job/team-memory-migrate`; the Job is kept on failure), fix the migration in a new release, and upgrade again. Do not edit `goose_db_version` by hand.

### A migration succeeded but is wrong

Roll back the pods first (`helm rollback`) if the new release misbehaves. Then fix the data or schema with a **new forward migration**. If data was destroyed, restore it from backup into a scratch database and copy the affected rows back ([backup-restore.md](backup-restore.md), point-in-time restore to just before the upgrade); a whole-database point-in-time restore is the last resort because it loses every write since.

## Drill (2026-10-22, disposable k3d cluster)

`deploy/dev-k3d/rollback-drill.sh` with a probe sending one MCP search through the ingress every 100 ms:

| Step | Result | Failed probes |
| --- | --- | --- |
| Install stage 05 (0.5.0, schema 6) | writes OK | – |
| Upgrade to 0.6.0 (migration 00007) | writes OK, schema 7 | 3 of 65 (one 502, two connection errors) as the **0.5.0** pods terminated |
| `helm rollback` to 0.5.0 | the 0.5.0 binary reads and writes on schema 7 | **0** of 80 |
| Upgrade to 0.6.0 again | writes OK | 3 of 72, again while 0.5.0 pods terminated |
| Upgrade to an image with a failing migration | hook failed, revision marked failed, Deployment still 0.6.0, no partial column, writes OK | **0** of 130 |
| Two rolling restarts of 0.6.0 (separate run) | | **0** of 282 |

The errors occur only when **0.5.0** pods stop: 0.5.0 closes its listener at SIGTERM while the ingress still routes to it. 0.6.0 fails readiness first, keeps serving for `api.shutdownDelay` (5 s), then drains within `api.shutdownTimeout` (20 s), and loses no request. The upgrade from stage 05 therefore costs about 0.3 s of errors per terminating old pod; later upgrades should cost none.
