# Decision: trust levels, review, overrides and promotion

Status: accepted (stage 04, 2026-10-21). Implements `review-decisions.md`. Enforced in Go (`internal/memory/review.go`, `promotion.go`) and in the database (`migrations/00003_security.sql`). Tests: `integration/security/review_test.go`, `promotion_test.go`.

## Trust levels

| Trust | Meaning | Set by |
| --- | --- | --- |
| `unverified` | Recorded by a person or agent; nobody vouched for it. | every new revision without a citation |
| `cited` | A `source_uri` was supplied and passed syntax and scheme checks (http/https, no credentials). It was **not** fetched; it may not exist or support the claim. | every new revision with a citation |
| `approved` | A maintainer of the owning scope reviewed **this exact revision** in a browser review. It does not mean the service verified the claim. | `Approve` only |

**Who may set `approved`:** only a human principal in a portal session (`AuthMethod = portal`), with maintainer on the owning scope. MCP calls and personal-access-token or OAuth bearer calls can never approve, even with a human maintainer's token. A service principal can never approve. In the database, the `approvals` INSERT policy requires the same conditions, and the memories guard trigger allows `trust = approved` only when the latest review outcome of the current revision is an approval.

## Rules

1. **New memories** start `unverified`, or `cited` with a valid `source_uri`. This holds for agent and human writers alike.
2. **Any new revision resets trust** to `unverified`/`cited`, including a revision of an approved memory. Its previous approval stays in history, bound to the old revision. The memory re-enters the review queue.
3. **`memory_approve` requests review only.** It needs maintainer on the owning scope and the current revision. It records one `review_requested` event per revision; repeats are no-ops. It never changes trust. Its result always says `approved: false` unless that revision was already approved in the portal. Writers' new revisions enter the queue without a request.
4. **Approval** (`Approve`) records an `approved` event and an `approvals` row (reviewer, time, exact revision, note), and sets trust to `approved`. It requires the revision the reviewer saw to still be current, and optionally the state version (stale means conflict). Approving an approved revision is a no-op. Approval rescans the content for secrets.
5. **Rejection** (`Reject`) records a revision-bound `review_rejected` outcome. The revision leaves the review queue and stays visible as unapproved knowledge (archive it to hide it). Rejecting the approved current revision **withdraws** the approval: trust returns to `cited`/`unverified`. A new revision re-enters review.
6. **Archive and supersede** need maintainer on the owning scope. They append events and never change content revisions. Superseding an approved memory does not carry approval to the replacement, which starts `unverified`/`cited`.
7. **Review queue** (`ReviewQueue`): active memories of one scope whose current revision has no review outcome and is not approved. Explicit requests sort first. Revisions that propose overrides are flagged.
8. **Retrieval** always reports `trust`. Search, recent and resources also report `approved_by`/`approved_at`. `memory_get` reports `review` (decision, reviewer, revision, time) for the current revision.

## Overrides

1. Override links are part of a content revision. Creating or changing them creates a new revision.
2. A link may name only a memory the author can read that is owned by a **strict ancestor** of the overriding memory's scope. Siblings, descendants, the same scope, other organizations and unreadable memories are refused, in Go and by the `revision_overrides` INSERT policy.
3. A link is **effective** only while the overriding memory is active, its current revision carries the link, and that revision's latest review outcome is an approval. Updating the overriding memory resets trust and disables the override until reapproval. Archiving, superseding or rejecting the overriding memory also disables it.
4. Effectiveness is rechecked on every search across the reader's ancestor chain, whether or not the overriding memory matches the query (`EffectiveOverrides`). An override in project A has no effect in sibling B, the team or the organization.
5. Before approval, views label the override `override_status: proposed` and both memories are returned. Afterwards it is `approved`, and the overridden memory is hidden from that scope's readers unless they ask for `include_overridden`.
6. Locked (non-overridable) organization guidance remains deferred.

## Promotion

Promotion moves an approved memory up **exactly one level** with consent on both sides:

1. **Submission** (`SubmitPromotion`, portal only): a human maintainer of the source scope submits the **approved current revision**, consenting to share its snapshot with the parent scope's reviewers. The snapshot holds content, rationale, citation, kind, revision, the source scope's path and the submitter's name. Submitting the same revision again returns the live request. The database allows only the parent scope as the destination.
2. **Visibility:** the request is visible to its submitter and to maintainers of the source or destination scope. Destination maintainers see the snapshot only: no source history, no other child memories, no child membership, and no source identifiers (redacted in the request view). The grant on the destination never authorizes reading or superseding child memories (RLS, tested through SQL).
3. **Finalization** (`FinalizePromotion`, portal only): a human maintainer of the destination accepts. In one transaction:
   - Go rechecks destination authority and rescans the snapshot for secrets;
   - it creates the parent memory from the snapshot (revision 1, `promoted_from` = source and revision), with a `promotion_finalized` event and a destination approval (trust `approved`);
   - `tm_promotion_finalize_source` (a definer function) rechecks the browser review, the human reviewer, destination maintainer, parent relationship, source still active at the submitted revision, the latest outcome of that revision still an approval, and the submitter still a maintainer of the source;
   - it then supersedes the source with reason `promotion` and marks the request accepted.

   Repeating an accepted finalization returns the result.
4. **Stale requests** (source changed, archived, approval withdrawn, submitter lost authority or was disabled) fail with `ErrPromotionStale` and are marked `stale`; the submitter must resubmit. Rejected and cancelled requests have no effect. The destination may reject (portal only). The submitter or a source maintainer may cancel.
5. **One person with both authorities** may submit and accept in one explicit browser action (`PromoteDirect`). Agents may not.
6. **Provenance redaction:** `promoted_from` and `superseded_by` links to memories the reader cannot see are removed from views. The new parent audience reads the approved snapshot only. The promoted memory's change note says only "promoted from a child scope": the source scope's path is in the request snapshot for destination reviewers, never in the memory that every reader of the destination (including sibling scopes) sees.

## What enforces "browser review"

`AuthMethod = portal` is set only by the portal adapter (stage 05) for a cookie session with CSRF protection. Stage 04 provides and tests the domain services. The browser flow and its end-to-end tests land in stage 05. The marker is a workflow boundary, not proof against browser automation, and the app role sets it itself (see `docs/decisions/rls.md`, residual risks).
