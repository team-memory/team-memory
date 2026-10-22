# Decision: git mirror security (specification for stage 06)

Status: accepted (stage 04, 2026-10-21). The mirror itself is built in stage 06; this record fixes what it may export and to whom. It follows `review-decisions.md` ("Revisions, lifecycle events, and mirror" and "Pilot and mirror audience").

## Audience

- **Operator-only in v1.** Only trusted deployment operators and the export worker get repository access. Users browse history through Team Memory, which applies permissions. Scope membership never grants git access. No synchronization between scope memberships and git-provider ACLs is built in v1.
- Operators are trusted with all exported scope content. A git copy offers no application-level revocation: removing someone from a scope does not remove what they already cloned, which is why users get no clone access.
- The export worker uses its own database role, a variant of `team_memory_worker` with SELECT on `memory_events`, `memory_revisions` and `scopes` (added in stage 06), plus a deploy credential per repository with push access only. Rotate both like any secret. Document repository administration in the stage 06 runbook.

## Repository mapping

- **One repository per scope that owns memories**, named from the scope's immutable ID (with the path in its README). Scopes cannot move, so the mapping is stable.
- **Inherited memories are not copied** into descendant repositories: a memory appears only in its owning scope's repository. A promotion produces two independently retriable exports: the source supersession in the source repository, and the new memory in the parent's.
- `mirror_state` is keyed by (event ID, repository) with commit SHA and delivery state. Commits are serialized per repository, and retries are idempotent by event ID.

## What may be exported

Per exportable event, one commit, built from the event's own snapshot and the revision it is bound to (never current state at export time):

| Exported | Never exported |
| --- | --- |
| memory ID, kind, title, content, rationale, `source_uri` | audit events (`audit_events`) |
| revision number, change note | authentication data: principals' identities, tokens, OAuth state, IdP subjects |
| trust at the time of the event; approver principal ID and display name, approval time, the approved revision | review notes on rejections (reviewer free text) |
| author principal ID and display name | scope memberships, roles, `scope_access` |
| lifecycle event type, event ID, timestamp, supersession and override links | conflict candidates, embeddings, jobs |
| promotion link to the source **only in the source scope's repository** | promotion request snapshots (they hold another scope's content) |

Display names are personal data. The stage 06 runbook must say how a name change or a purge is handled. Event commits are not rewritten for renames; purges follow the procedure below.

## Gate: secret scanning before export

The export worker rescans every field it writes with `internal/secrets` (the same rules as writes, at the version deployed then). If the scan finds anything, the event is not exported: it is marked `failed` with reason `secret_detected`, an operator is alerted, and later events of that memory are held. Writes already reject detected secrets, so this catches revisions written before a rule existed. The purge procedure then applies.

## Archive and purge

- **Archive** is a lifecycle event like any other. It is exported as a commit that moves the memory's file under `archived/` (or sets `status: archived` in frontmatter; stage 06 picks one) and keeps history. Archive is not deletion. *Stage 06 chose `status: "archived"` in frontmatter with the file left in place, so `git log -- <file>` shows a memory's whole life (docs/operations/mirror.md).*
- **Purge** (accidental secret, legal request) is an exceptional, operator-authorized procedure. It is outside application privileges and defined in the stage 06 runbook. For the mirror it means: rotate the exposed credential first; rewrite the affected repository's history to drop the content (go-git cannot force-push to every host's protection rules, so the runbook names the manual steps); delete and recreate the remote if the host keeps unreachable objects; and record which clones and backups may still hold it. Copies already downloaded cannot be recalled, and the runbook says so.
- After a purge, the database marks the affected events as purged, so a redelivery cannot re-export them.

## Not covered

PR-based approval through git, importing from git (only explicit disaster recovery may, without restoring authoritative approval records), and per-user mirror access are out of scope for v1.
