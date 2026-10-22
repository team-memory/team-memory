# Incident basics

## First five minutes

1. **What is broken?** `kubectl -n team-memory get pods`; `/readyz` (port-forward, see [authentication.md §6](authentication.md#6-check-readiness)); the alert that fired.
2. **Since when, and what changed?** `helm history team-memory -n team-memory`, recent Secret or ConfigMap changes, IdP or database maintenance.
3. **Stop the bleeding**: a bad release is rolled back with `helm rollback` ([upgrade.md](upgrade.md)); the schema stays.
4. **Tell users** the MCP endpoint is degraded. Agents keep working without Team Memory; nothing is lost except new writes during the outage.
5. **Keep evidence**: logs (`kubectl logs --since=1h`), request ids from user reports (`X-Request-Id`, equal to the trace id), traces, audit events (`team-memory audit list --scope ...`).

## Common situations

| Symptom | Likely cause | Action |
| --- | --- | --- |
| All API pods not ready, `/readyz`: `database unavailable` | PostgreSQL down, network policy, password | Check the database host; [backup-restore.md](backup-restore.md) if it is lost |
| Not ready with an `sso:` reason | IdP Secret, IdP down, discovery | [authentication.md §11](authentication.md#11-troubleshooting-a-server-that-is-not-ready). The server refuses requests rather than weakening authentication: by design |
| 401 for everyone | Signing key changed without the two-step rotation; tokens expired | Restore the previous key into the Secret and restart |
| 429 for many users | Rate limits shared behind the ingress | Set `trustedProxies` |
| Search slow, `TeamMemorySearchSlow` | Ollama slow (query embedding: see the `embedding_duration_seconds{purpose="query"}` metric); database load | Search falls back to full text after `TEAM_MEMORY_QUERY_EMBED_TIMEOUT` (2 s); check pool saturation and slow queries |
| Backlog or dead letters | Worker dependency down | [dead-letters.md](dead-letters.md) |
| `TeamMemoryMirrorSecretDetected` | A credential in a memory | [secret-purge.md](secret-purge.md), rotate first |
| WAL archiving failing, disk filling | Repository unreachable or full | Fix the repository **before** the disk fills; PostgreSQL stops accepting writes when it does |
| Suspected data leak between scopes | | Treat as a security incident: preserve audit logs and database state, disable affected principals (`principal disable`), and report it privately to the maintainers |

## Afterwards

Write a short note: timeline, impact (requests failed, data lost if any), cause, what detected it, what will change. Keep it with the operations records, not in the memory service.
