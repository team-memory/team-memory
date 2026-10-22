# Dead letters: inspect and replay

Background work (embeddings: `embed`; git mirror: `git_mirror`) runs from the `jobs` table. A failed attempt is retried with exponential backoff (5 s doubling to 10 min, ±20% jitter); after `max_attempts` (10, about 40 minutes of failures) the job is **dead-lettered**. A worker that disappears mid-job releases it when its lease (5 min) expires. Dead jobs never block other work, and the service keeps answering: search falls back to full text for memories without embeddings, and the mirror simply lags.

Operator commands run with the operator role, for example through `deploy/helm/team-memory/scripts/tm-admin.sh <namespace> <release>`:

## 1. Notice

Alerts `TeamMemoryDeadLetters` (any dead job), `TeamMemoryJobBacklog` (oldest ready job older than 15 minutes), `TeamMemoryWorkerStalled`. Or:

```sh
tm-admin.sh team-memory team-memory jobs status
```

## 2. Inspect

```sh
tm-admin.sh team-memory team-memory jobs dead --kind embed
tm-admin.sh team-memory team-memory jobs dead --kind git_mirror
```

Each row shows the job id, attempts, when it died, its payload (memory and revision, or event and repository) and the last error. Also read the worker's log around that time (`kubectl -n team-memory logs deploy/team-memory-worker` or `-mirror`), searching for the job id.

| Last error | Cause | Fix before replaying |
| --- | --- | --- |
| `embedding provider unavailable`, timeouts | Ollama down or overloaded | Bring Ollama back; check `networkPolicy.embeddings` |
| `dimension mismatch` | model changed under the same name | Register the model properly (`embeddings add-model`) |
| `mirror remote unavailable: ... authentication` | deploy key or token revoked or rotated wrongly | Fix the Secret ([mirror.md](mirror.md)) |
| `... repository not found` | the scope's mirror repository was never created | Create it (named after the scope id) |
| `secret_detected: ...` | a credential in a memory | **Do not replay.** Follow [secret-purge.md](secret-purge.md) |
| `lease expired` repeatedly | the worker is killed mid-job (OOM, eviction) | Check pod restarts and memory limits |

## 3. Replay

After fixing the cause:

```sh
tm-admin.sh team-memory team-memory jobs retry-dead --kind embed
tm-admin.sh team-memory team-memory jobs retry-dead --kind git_mirror
```

This resets every dead job of that kind to pending with a fresh attempt budget. Handlers are idempotent: an embedding that already exists is not recomputed, and a mirror event already in the remote history is acknowledged without a new commit. Watch `jobs status` until the backlog drains.

Mirror events that are `held` or `failed` (not dead jobs) are requeued with `team-memory mirror requeue --repo <scope-id>` once their cause is resolved.

## 4. Verify

`jobs status` shows no `dead` rows; `TeamMemoryDeadLetters` resolves; for the mirror, run the verify Job (`kubectl -n team-memory create job --from=cronjob/team-memory-verify-mirror verify-now`).
