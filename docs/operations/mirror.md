# Git mirror operations

The git mirror is a one-way, operator-only export of memory history. PostgreSQL is the source of truth; the mirror gives operators browsable history, diffs and supplementary off-site recovery material. It is **not** a database backup (see [backup-restore.md](backup-restore.md)) and it is **never imported** during normal operation. What it may contain is fixed by [docs/decisions/mirror-security.md](../decisions/mirror-security.md); the library choice and its limits are in [docs/decisions/git-library.md](../decisions/git-library.md).

## What gets exported

One commit per exportable lifecycle event, built from the event's own snapshot and the immutable revision it is bound to, never from current state:

| Event | Commit subject | Exported |
| --- | --- | --- |
| `created` | `create <kind> <title> (revision 1)` | yes |
| `revised` | `update <kind> <title> (revision N)` | yes |
| `approved` | `approve ...` | yes |
| `archived` | `archive ...` (file stays, `status: "archived"`) | yes |
| `superseded` | `supersede ...` (`superseded_by`, `supersede_reason`) | yes |
| `promotion_finalized` | `promote ...` in the **parent** scope's repository | yes |
| `review_requested`, `review_rejected`, `promotion_submitted` | | no (reviewer notes and promotion consent stay in the database) |

Files live at `memories/<kind>/<memory-id>.md`: JSON-valued YAML frontmatter (event id and sequence, memory id, scope, revision, kind, status, trust, `source_uri`, memory author, revision author, change note, approver and the revision approved, supersession, override links and whether they were effective, `origin: "promotion"`), then the title, content and a `## Rationale` section. A kind change moves the file.

The commit author is the principal who acted (display name, `<principal-id>@<TEAM_MEMORY_MIRROR_EMAIL_DOMAIN>`). Real email addresses are never exported. The memory's and revision's authors are recorded in the frontmatter. Every commit ends with `Team-Memory-Memory`, `Team-Memory-Event` and `Team-Memory-Event-Seq` trailers.

**Promotion** produces two independent jobs. The parent repository gets a `promote` commit (`origin: "promotion"`, no reference to the child scope: the decision record allows the source link only in the source repository). The child repository gets a `supersede` commit with `supersede_reason: "promotion"` and `superseded_by` naming the new parent memory.

**Archive** keeps the file and sets `status: "archived"`, so `git log -- <file>` shows the whole life of a memory.

## Delivery guarantees

- Exportable events enqueue a `git_mirror` job and a `mirror_state` row (event, repository) from a database trigger, in the same transaction as the event. Nothing is enqueued while the mirror is disabled.
- The worker exports a repository under a PostgreSQL advisory lock, so commits and pushes are serialized per repository even with several workers. It exports every undelivered event of that repository in event order, then pushes once.
- Before committing, it walks the remote history back to the oldest undelivered event and skips events whose trailer is already there. A retry after a push that succeeded but was never acknowledged therefore writes nothing new.
- A push rejected as non-fast-forward (someone else pushed) is resynced and rebuilt once, then retried with backoff like any failure. After `max_attempts` (10) the job is dead-lettered; `mirror_state.last_error` says why. The service keeps working while the remote is down.
- Every field is rescanned for secrets with the deployed rules before export. A match marks the event `failed` (`secret_detected`), dead-letters its job, logs an error and increments `team_memory_mirror_secret_detected_total`. Later events of the same memory are `held`, so the file never moves past a blocked revision. Follow [secret-purge.md](secret-purge.md).

## Setup

1. **Create one private repository per scope that owns memories**, named after the scope's id. List them with:

   ```sh
   team-memory scope list            # ids and paths
   ```

   The remote URL template maps a repository key (scope id) to a URL, for example `ssh://git@git.example.com/team-memory-mirror/{repo}.git`. Put the scope path in each repository's description or README. Scopes cannot move, so the mapping never changes. Create a repository before the first memory of a new scope is exported; until then, its jobs fail and retry (and dead-letter after about 40 minutes with the default backoff, after which `jobs retry-dead --kind git_mirror` resumes them).

2. **Create a credential with push access to those repositories only**:
   - SSH (preferred): a deploy key per repository, or one machine-user key limited to the mirror organization or group. Record the server's host key in a `known_hosts` file from a trusted channel (the git host's documentation or its admin console), not from a first connection.
   - HTTPS: a fine-grained token limited to the mirror repositories with contents write. Add the CA bundle if the server uses a private CA.

   Never grant mirror access based on scope membership. Only trusted deployment operators and the export worker get repository access.

3. **Store the credential in a Kubernetes Secret** (see the chart's `mirror.existingSecret`), with keys `ssh-privatekey` and `known_hosts`, or `token` (and optionally `ca.crt`). The worker reads the files on every export, so a rotated Secret takes effect within the kubelet's sync period (about a minute) without a restart.

4. **Create the database login** for the export worker and put its URL in the worker Secret:

   ```sh
   team-memory db create-login --role mirror --user tm_mirror   # prints a generated password once
   ```

5. **Enable the chart's mirror worker** (`mirror.enabled=true`, `mirror.remote=...`) and deploy.

6. **Enable exports**, which also backfills every earlier event in order:

   ```sh
   team-memory mirror enable
   team-memory mirror status
   ```

## Settings

| Variable | Meaning |
| --- | --- |
| `TEAM_MEMORY_MIRROR_DATABASE_URL` | login role in `team_memory_mirror` (worker only) |
| `TEAM_MEMORY_MIRROR_REMOTE` | URL template with `{repo}`; `ssh://`, `https://` or `file://` |
| `TEAM_MEMORY_MIRROR_BRANCH` | default `main` |
| `TEAM_MEMORY_MIRROR_CACHE_DIR` | bare clones (disposable; an `emptyDir` in the chart) |
| `TEAM_MEMORY_MIRROR_SSH_KEY_FILE`, `TEAM_MEMORY_MIRROR_SSH_KNOWN_HOSTS_FILE` | SSH deploy key and pinned host keys |
| `TEAM_MEMORY_MIRROR_TOKEN_FILE`, `TEAM_MEMORY_MIRROR_TOKEN_USER`, `TEAM_MEMORY_MIRROR_CA_FILE` | HTTPS token (password), user name (default `x-access-token`), CA bundle |
| `TEAM_MEMORY_MIRROR_EMAIL_DOMAIN` | domain of author addresses (default `users.noreply.team-memory.invalid`) |
| `TEAM_MEMORY_WORKER_*` | batch, poll, lease and backoff, shared with the embedding worker |

Credentials are paths to files, never values in the environment, and are never logged.

## Verify

```sh
team-memory mirror verify            # exit status 3 when drift is found
team-memory mirror verify --json
```

`verify` compares every repository with the database. For each memory, it renders the latest event acknowledged as pushed and compares it with the file in the repository. It reports:

| Kind | Meaning |
| --- | --- |
| `undelivered` | the memory's latest exportable event is pending, committed but unacknowledged, failed, held or never enqueued |
| `missing_file` | an event was acknowledged but its file is gone (deleted in git) |
| `content_differs` | the file is not what the database renders (edited in git, or a newer foreign commit) |
| `unexpected_file` | a file no pushed event produces (added in git, or a purged memory) |
| `foreign_commit` | a commit without mirror trailers (someone pushed by hand) |

A display-name change since export is reported as a note, not drift, because commits are not rewritten for renames. Run `verify` after upgrades, after restores, and on a schedule (the chart's optional `mirror.verifyCronJob`).

## The mirror is read-only by convention

Edits made in git are **never imported**. The next export of an edited memory overwrites its file with the database rendering, and `verify` flags the foreign commit until history is cleaned. To change knowledge, use Team Memory. To undo a change, create a new revision or supersede the memory through the service; `git revert` in the mirror changes nothing in Team Memory and is overwritten. Git history is for review and disaster recovery only.

## Repository access administration

- Grant clone access only to named deployment operators. Review the list when someone leaves the operator group, and rotate the mirror credential at the same time.
- Removing someone from a scope does not remove what they cloned from the mirror. That is why users never get mirror access.
- **Rotate the credential**: add the new deploy key or token alongside the old one at the git host, update the Kubernetes Secret, wait for one successful export (`mirror status`, or the `team_memory_mirror_exports_total` metric), then remove the old credential at the host. See [rotate-secrets.md](rotate-secrets.md).
- **Display names** are personal data. A rename appears in later commits only; earlier commits keep the old name. Removing a name from history is a purge ([secret-purge.md](secret-purge.md)).

## Operations

| Task | Command |
| --- | --- |
| Status per repository | `team-memory mirror status` |
| Dead-lettered jobs (remote down too long) | `team-memory jobs status`, then `team-memory jobs retry-dead --kind git_mirror` |
| Held and failed events after a purge or rule fix | `team-memory mirror requeue --repo <scope-id>` |
| Never export a memory again (purge) | `team-memory mirror mark-purged --memory <id>` |
| Stop enqueuing (exports already queued still run) | `team-memory mirror disable` |

## Disaster recovery: rebuild from the mirror

This is the **only** sanctioned import, for total loss of the database **and** its backups. Restore from a PostgreSQL backup whenever one exists ([backup-restore.md](backup-restore.md)); the mirror is a fallback that recovers knowledge content only.

What comes back: the content of every non-superseded memory at the mirror's head (title, content, rationale, kind, source URI), optionally archived memories (recreated, then archived).

What does **not** come back, by design or because git does not have it:

- **Approvals.** Every rebuilt memory starts unapproved (`cited` if it has a source URI, else `unverified`). Earlier approval metadata appears only in the change note, as history. A maintainer must review again in the portal.
- Revision history (each memory gets one new revision), memory ids (new ids; the original id is in the change note), override links (they referenced old ids; re-create them), promotion links.
- Audit events, review notes, memberships and roles, principals and identities, tokens, OAuth state, embeddings (the worker re-creates these), conflict candidates and jobs.
- Events after the last successful push (the RPO of the mirror is its delivery lag; see `team_memory_mirror_undelivered`).

Procedure:

1. Bring up a fresh database and run `team-memory init` (or recreate the organization with the operator CLI). Recreate the scope tree and memberships from your own records.
2. For each lost scope, create its replacement scope, then:

   ```sh
   export TEAM_MEMORY_MIRROR_REMOTE=ssh://git@git.example.com/team-memory-mirror/{repo}.git  # and credential settings
   team-memory mirror rebuild --repo <old-scope-id> --scope <new-scope-path> --as <principal-id> --dry-run
   team-memory mirror rebuild --repo <old-scope-id> --scope <new-scope-path> --as <principal-id> [--include-archived]
   ```

   `--as` must be a writer in the new scope (and a maintainer with `--include-archived`). Every restored memory is attributed to that principal.
3. Ask maintainers to review the restored memories in the portal.
4. The new scopes have new ids, so they get new mirror repositories. Archive the old repositories read-only; never push the rebuilt history into them.

The procedure was exercised in `TestMirrorDisasterRecoveryRebuild` (integration suite) and once by hand against the disposable cluster (see [restore-drill-report.md](restore-drill-report.md)).
