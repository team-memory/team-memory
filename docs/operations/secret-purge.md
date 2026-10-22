# Exceptional purge (accidental secrets, legal requests)

History in Team Memory is immutable **under normal operation**: no application role can change or delete a revision, event, approval or audit row. A purge is the exceptional, operator-authorized remediation for text that must not remain, typically a credential pasted into a memory. It runs outside application privileges, as the database owner, and is audited.

Be honest with the requester about the limits: **copies already downloaded cannot be recalled.** Anyone who read the memory, cloned the mirror, or holds a backup or a restored copy may still have it. The purge reduces where the text lives from now on.

## 0. Rotate first

If the text is a credential, revoke or rotate it **before anything else**. A purge without rotation only hides a key that is still valid. Record when it was rotated.

## 1. Authorize and identify

- Get explicit authorization from the owner of the deployment (record a ticket or reference; it goes into the audit event).
- Identify the memory id(s): the portal, `memory_search`, or the mirror alert (`mirror status`, `jobs dead --kind git_mirror` shows `secret_detected` with the event and memory).
- Note where the text may also be: promotions (the parent memory created by promoting it), copies the author made in other memories, review notes.

## 2. Database content, events, embeddings, jobs

Run `deploy/operations/purge-memory.sql` as the table owner (the role that ran the migrations), once per memory:

```sh
psql "$OPERATOR_URL" -v memory_id=<uuid> -v ticket='<authorization ref>' -f deploy/operations/purge-memory.sql
```

In one transaction it:

- replaces the title, content, rationale, source URI and change notes of **every revision** of the memory with `[purged]` markers (the full-text index is recomputed);
- redacts review notes on its approvals and the snapshot of promotion requests that quote it;
- deletes its embeddings and conflict candidates, and closes its pending jobs;
- marks its mirror events `purged`, so no worker exports them again;
- archives the memory (out of default retrieval) and appends a `memory.purge` audit event with the authorization reference;
- disables the immutability triggers only inside the transaction and verifies they are active again before committing.

Rows and ids are kept, so revision numbers, events, approvals and links stay consistent. Lifecycle events never contained content. Audit events never contain memory content. Tested by `TestPurgeRunbookSQL`.

If the memory was promoted, purge the parent memory too (its id is in the source's `superseded_by`).

## 3. Git mirror

The mirror may already contain the text in history (the export rescans with the current rules, but a rule may have been added later).

1. The database step already marked the events purged. Confirm: `team-memory mirror status`.
2. On an operator workstation (the service has no git binary; operators may use their own tools), rewrite the affected repository's history to drop the file's sensitive versions, for example with `git filter-repo --path memories/<kind>/<memory-id>.md --invert-paths` on a fresh mirror clone, then force-push. Branch protection on the git host may need to be lifted temporarily.
3. Many hosts keep unreachable objects and cached views (pull request diffs, forks, caches). If yours does, **delete and recreate the repository** and push the rewritten history to it, or ask the host's support to purge.
4. Every operator clone of that repository still has the text: ask each operator to delete their clone and re-clone.
5. Run the verify Job; expect `unexpected_file` to be gone and no drift.
6. If the purge made other events of the memory `held`, they stay unexported (they are purged too).

## 4. Backups and restored copies

PostgreSQL base backups and archived WAL are immutable files; the text stays in every backup and WAL segment taken while it existed.

1. After the purge, take a **new full backup**: `pgbackrest --stanza=tm --type=full backup`.
2. Decide the retention to apply. With `repo1-retention-full=2`, older backups and their WAL expire after two more full backups (about two weeks with weekly fulls). To remove them sooner, lower retention temporarily and run `pgbackrest --stanza=tm expire --repo1-retention-full=1`; this also drops your ability to restore to points before the purge.
3. Object storage with versioning or object lock keeps deleted backup files until the lock expires; record when that is.
4. Delete scratch databases and restored copies from restore drills taken before the purge.
5. Record which copies may still hold the text and until when.

## 5. Display names and other personal data

A principal's display name in mirror commits is personal data. A rename appears in later commits only. Removing a name from history is a mirror purge (step 3) for every repository that has commits by that principal, plus the backup considerations in step 4.

## 6. Close out

- Audit trail: `team-memory audit list --scope <scope>` shows `memory.purge`.
- Write down: what was exposed, since when, who could read it (scope readers, mirror operators, backup holders), rotation time, purge time, remaining copies and their expiry.
- Consider adding a scanner rule (`internal/secrets`) if the credential format was missed. Note that **review notes are not scanned** today (found while testing this runbook).
