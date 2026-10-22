# Rotate secrets

Rotate on a schedule, when someone with access leaves, and **immediately** after any suspected exposure. Every secret lives in a Kubernetes Secret (or comes from an external secret operator) and is referenced by name; none is in `values.yaml`, the image or git.

| Secret | Kubernetes Secret (keys) | Restart needed | Procedure |
| --- | --- | --- | --- |
| IdP client secret | `team-memory-idp-<id>` (`client-secret`) | no (read on every use) | [authentication.md §10](authentication.md#10-secret-rotation-without-downtime) |
| Token-signing key | `team-memory-signing` (`<kid>.pem`) | two rolling restarts | authentication.md §10 |
| Database passwords | `team-memory-db` (`operator-url`, `app-url`, `worker-url`, `mirror-url`) | rolling restart of that component | below |
| Mirror deploy key or token | `mirror.existingSecret` | no (read on every export) | [mirror.md](mirror.md#repository-access-administration) |
| TLS certificate | `ingress.tls.secretName` | no (the ingress reloads it) | your certificate manager |
| Personal access tokens | in the database (hashed) | no | `token create`, then `token revoke <id>` of the old one |
| pgBackRest repository passphrase | on the database host | no | [backup-restore.md](backup-restore.md) |

## Database passwords without downtime

Use a second login for the same group role, switch to it, then drop the first:

```sh
# 1. new login (prints its password once)
tm-admin.sh team-memory team-memory db create-login --role app --user tm_app_2
# 2. point the Secret at it
kubectl -n team-memory patch secret team-memory-db --type merge \
  -p '{"stringData":{"app-url":"postgres://tm_app_2:NEW_PASSWORD@db.example.internal:5432/teammemory?sslmode=require"}}'
# 3. roll the component; old pods keep their connections until they stop
kubectl -n team-memory rollout restart deploy/team-memory-api && kubectl -n team-memory rollout status deploy/team-memory-api
# 4. remove the old login (as the database owner)
psql "$OPERATOR_URL" -c 'DROP ROLE tm_app'
```

Same for `worker` (`deploy/team-memory-worker`) and `mirror` (`deploy/team-memory-mirror`). For the operator URL, change the owner's password at a quiet time (`ALTER ROLE ... PASSWORD`), update `operator-url`, and verify with `tm-admin.sh ... jobs status`; only the migration Job and the CLI use it.

## After an exposure

1. Rotate the exposed credential first (above).
2. Contain what it could be used for. A leaked signing key can forge access tokens until it is removed from `team-memory-signing` (do steps 1–3 of the key rotation without waiting the 15 minutes). A leaked personal access token: `token revoke <id>`. If a person's account may be misused, `principal disable <id>` blocks all of their credentials at the next request until re-enabled. A leaked database password: also check `pg_stat_activity` and the audit log (`audit list`).
3. If the credential was written into a memory, follow [secret-purge.md](secret-purge.md).
