# Accepted review decisions

These decisions are part of the implementation contract. Read them with the proposal and every stage prompt. They take precedence over older draft wording. Owner bootstrap is decided: CLI initialization for v1, with explicit authenticated SSO linking.

## Human approval and effective overrides

- Approval and promotion require an explicit authenticated browser review with CSRF protection, bound to the exact revision. MCP and personal-access-token API calls cannot finalize either action, even when the token belongs to a human maintainer. Solo users may sign into the portal using a token, then review explicitly. A principal-type flag alone does not prove human involvement; the browser interaction is a workflow boundary, not proof against browser automation.
- Keep the eight-tool MCP contract: `memory_approve` requests review of an exact revision; it never marks a memory approved. Make this behavior explicit in its description and result. Duplicate requests are idempotent. Only authorized maintainers can request review through this tool in v1; writers' proposals already enter the review queue on creation/update.
- An override takes effect only when its exact content revision and override link have been approved together. Creating or changing the link creates a new revision requiring approval. Before approval, show both memories and label the proposed override. Updating an approved overriding memory resets its trust and disables its override until reapproval; archived or superseded overrides are ineffective. Recheck effectiveness before ranking, across the target scope's ancestor chain, independent of whether the overriding memory matches the search query. Siblings and ancestors are unaffected.
- `cited` means a source URI was supplied and validated for syntax/scheme only; it does not mean the source was fetched, is accurate, or supports the claim. `approved` means a maintainer reviewed an exact revision, not that the service verified the claim.

## Promotion consent and visibility

A source maintainer submits a promotion request bound to an approved source revision, explicitly consenting to share that snapshot with destination reviewers. The request exposes only the submitted content, rationale, citation, revision, and minimal provenance necessary for review, not source history, other child memories, or child membership. Destination reviewers receive narrowly scoped access to the request snapshot; this does not grant access to the source scope. Redact inaccessible provenance links and scope names on public-facing memory views.

A destination maintainer accepts through browser review. If one person has both source and destination authority, submission and acceptance may be one explicit browser action. A destination grant alone never authorizes reading or superseding arbitrary child memories. Finalization transactionally rechecks source revision/status/approval, the source submitter's continuing authority, destination reviewer authority, parent relationship, and secret-scan result. Stale requests require resubmission; rejected/cancelled requests have no effect. Finalization creates a new parent memory and destination approval, supersedes the original with reason `promotion`, and appends lifecycle/audit events atomically. It is idempotent, advances one level only, and never exposes source history to the new audience. The narrowly authorized source supersession must be enforced and tested through the database access path as well as Go checks.

## Revisions, lifecycle events, and mirror

Content revisions are immutable snapshots of content, rationale, citation, kind, and override links. Approval, review rejection, archival, supersession, and promotion are immutable lifecycle events bound to a revision; they do not mutate a historical content revision. Current status/trust may be a transactional projection of these records. Historical reads distinguish revision content from events and show what was approved at the time. Mutations use an expected state/version as well as an expected content revision where necessary to detect concurrent lifecycle actions.

The git mirror exports one commit per exportable event (including creation and revision events), identified by an immutable event ID. It must use the event's historical snapshot, not whatever state is current when a delayed worker runs. `mirror_state` is keyed by event and destination repository, with commit SHA and delivery state. Serialize commits/pushes per repository, not merely per memory; retries and recovery after a push succeeds but acknowledgement fails must not duplicate commits. Promotion produces independently retriable source and destination exports. Audit/authentication data remain excluded. The mirror is supplementary recovery material, not a complete database backup or a source of authoritative approval records after database loss.

## Lifecycle and administration ownership

- Stage 01: scope creation/listing and configuration CLI/service methods; initial membership bootstrap; immutable content revisions and lifecycle-event schema; archive/supersede service contracts. No public deletion endpoint.
- Stage 04: authorized scope/membership administration CLI/API, role/cascade changes and revocation, review-request and rejection operations, archival/supersession behavior, approval-gated overrides, promotion requests and finalization services. Trusted review completion is reserved for the portal adapter; test the domain services here, then browser flows in stage 05.
- Stage 05: explicit human review UI, rejection notes, approval and promotion finalization, and scoped request-snapshot views.
- Rejection records a revision-bound review outcome and removes that revision from the pending queue; it remains visible as unapproved knowledge unless a maintainer archives it. A new content revision re-enters review. Archival excludes a memory from default retrieval without deleting history. Supersession links to a replacement; neither action changes historical content revisions.
- Stage 06: exceptional administrative purge runbook for accidental secrets, covering database content/events, embeddings, jobs, mirror/history rewrites, retained backups and restored copies. Rotate exposed credentials first. Explicitly document retention limits and that copies already downloaded cannot be recalled. Purge is outside ordinary application privileges and requires explicit operator authorization; immutable means immutable under normal operation, not impossible to remediate.

## Pilot and mirror audience

The first private pilot is the author working on one project across multiple machines using the existing Kubernetes setup. It validates cross-machine continuity and client interoperability. Use separate test identities for authorization tests; reserve claims about team usability for a later multi-person pilot. No pilot project has been selected yet; choose it before the real pilot, without blocking foundation work. Team Memory itself is a possible dogfooding project, not a settled choice. Synthetic fixtures may be used for engineering tests but must not be presented as real pilot evidence.

The author has no self-hosted SSO yet. Use disposable local Keycloak for development/testing as already planned; production SSO remains optional for solo use.

The release priority is to launch on GitHub as soon as the agreed stability checks pass, with no fixed calendar deadline. Use the existing security, cross-client, recovery, clean-install and operational checks as the evidence; do not label failed or skipped required checks stable. Distinguish technical stability from unproven team adoption. Preparing a release does not itself publish it.

V1 git mirrors are operator-only: only trusted deployment operators and the export worker receive direct repository access. End users read permission-filtered history through Team Memory. Keep one repo per owning scope; do not implement synchronization between scope memberships and git-provider ACLs. Operators are trusted with exported data; document credential rotation and repository access administration.

## Owner bootstrap and identity linking

V1 uses `team-memory init`, run by an operator with server access, to establish the initial organization owner and issue the initial personal access token. Initialization must not reset or replace an existing owner when rerun. Neither the first arbitrary SSO login nor an email-domain match confers ownership. Preserve ownership when SSO is enabled.

To link SSO, the existing owner signs into the portal, explicitly chooses Connect SSO, and authenticates with the configured provider. Require proof of control of both identities and reject an identity already linked to another principal. Never silently link accounts by email alone. Stage 03 documents the flow and operator recovery procedure; stage 04 implements its authorization, and stage 05 supplies the browser UI.

Single-use browser setup tokens and preconfigured SSO-owner bootstrap are deferred beyond v1; do not reopen bootstrap selection during stage 03.
