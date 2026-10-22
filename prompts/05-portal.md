# Stage 05: Web Portal

## Objective
Build a web portal where signed-in people browse and search the memories visible at any scope they can access, see inherited memories clearly labeled, inspect history and provenance, and (for maintainers) review agent-proposed memories. Memories flow down the scope tree; the portal must never show descendants' memories at a higher scope, and the only way a memory moves up is an explicit, human-approved promotion. Where memories conflict, the lowest scope wins.

## Context
Team Memory is an open-source Go MCP service on PostgreSQL + pgvector giving AI agents and people shared, versioned, permission-aware knowledge (see `proposal.md`). Knowledge lives in a scope tree (default levels organization → team → project; admins may add named levels up to a configured depth). A scope's own memories are readable with membership there or a cascading grant on an ancestor; anyone who can read a scope also reads its ancestors' memories (inherited, read-only); nothing flows up. Stage 04 enforced this with row-level security and an adversarial suite, and stage 03 built the login layer (OAuth with upstream OIDC, or personal access tokens for solo use). Trust levels: `unverified`, `cited`, `approved`. The portal is v1 read, search, and review; free-form editing is out of scope.

## Inputs
- `prompts/00-start-here.md` (attach; follow its global rules and completion gates).
- `review-decisions.md` (attach with the proposal; required accepted security/lifecycle decisions).
- `proposal.md`, `docs/handoff/01-foundation.md`, `docs/handoff/02-retrieval.md`, `docs/handoff/03-identity.md`, `docs/handoff/04-security.md`, `docs/decisions/scope-tree.md`, `docs/decisions/trust-rules.md`, `docs/security/threat-model.md` (attach). Ask for anything missing.
- The repository root (the repository root), which holds both the service code and these planning files.

## Work
1. **Architecture decision** (`docs/decisions/portal.md`): default to server-rendered HTML with Go templates and htmx (or similarly light progressive enhancement), embedded in the server binary with no separate Node build or extra deployable, unless you find a concrete reason for a SPA; record alternatives and why. The portal calls the same `internal/memory` and `internal/search` services as MCP through a portal adapter (`internal/portal`), never raw SQL, and never bypasses RLS: every request runs in the principal's transaction.
2. **Browser authentication:** reuse the stage 03/04 login (authorization code with PKCE or the equivalent server-side session flow, against the configured upstream OIDC or, in solo mode, a personal-access-token sign-in page). Server-side sessions in secure, HttpOnly, SameSite cookies with idle and absolute timeouts, rotation on login, logout that invalidates server-side, CSRF protection on every state-changing request, and no tokens in URLs or local storage. Fail closed if SSO is configured but invalid. Implement Connect SSO for an authenticated existing owner, proving control of both identities, preserving principal/ownership, and rejecting an upstream identity already linked to another principal. In team mode, provide the explicitly authorized existing-owner authentication path needed for initial linking; it must not become an unauthenticated setup or bypass. Do not add browser owner bootstrap.
3. **Navigation and views:**
   - A scope tree navigator showing only scopes the principal can browse into (a scope they cannot read is not revealed, including its name).
   - Scope page: tabs for **This scope's memories** and **Inherited from ancestors** (grouped by origin scope, read-only, each linking to its origin). Never any "from descendants" aggregation. Counts shown must also respect visibility.
   - Filters: kind, trust level, status (active/superseded/archived), date; full-text and hybrid search (the same search service as MCP), with the same provenance fields.
   - Memory page: content, rationale, kind, trust level, origin scope, source link, author, approver, current revision, full revision history with diffs between revisions, and supersession chain.
   - Maintainer review queue per scope: unverified/cited agent proposals awaiting approval, with approve, reject (with a note), archive, and supersede actions following `review-decisions.md`. Approval/promotion finalization uses trusted browser-review services that MCP cannot invoke; all adapters share underlying authorization and lifecycle logic. Approvals bind to a revision; the page shows the exact revision being approved. Optimistic-concurrency conflicts surface as a clear message, not a silent overwrite.
   - **Approve and promote:** implement explicit browser review and the request workflow in `review-decisions.md`. A source maintainer approves an exact revision and consents to sharing its snapshot with parent reviewers; a destination maintainer accepts. One person holding both permissions can complete this in one browser action. Show destination, exact revision, secret-scan result, and authorized audience-impact information without exposing hidden membership counts/names. Destination-only reviewers see only the submitted snapshot and minimal provenance, never child history. Recheck consent/permissions and revision on acceptance; stale requests require resubmission. Show promotion links only where the viewer can access their targets, with redacted provenance otherwise. Promotion advances one level and supersedes the source atomically.
   - **Conflicts:** when search or the review queue finds a conflict candidate, show the inherited memory next to the lower-scope one with a "Mark as overriding" action available to writers on the lower scope. Marking an override creates a new revision awaiting approval; show both memories until that exact revision/link is approved. In normal views, effectively overridden inherited memories are collapsed under the overriding memory with a note ("Overridden at <scope>: lowest scope wins"); a toggle shows them. The review queue surfaces conflict candidates when a reviewer approves or promotes a memory that is highly similar to a memory in another scope, explaining that promoting it will make it inherited by every descendant, and that lower scopes may still override it.
   - Members view for scope maintainers (read-only listing of who has which role and whether it cascades); managing members stays in the authorized stage 04 CLI/API for v1.
4. **Rendering untrusted content safely:** memory content is untrusted data written by agents. Render as sanitized markdown or plain text with an allowlist sanitizer, no raw HTML, no script, link scheme allowlist (`https`, `http`, `mailto`, and the configured source schemes), `rel="noopener noreferrer"`, images disabled by default. Strict Content-Security-Policy (no inline script unless nonce-based), `X-Content-Type-Options`, frame denial, referrer policy, and no mixed content. Mark inherited and unverified memories visibly so a reader cannot mistake an agent proposal for team policy.
5. **Accessibility and usability:** keyboard navigable, readable at phone widths, dark and light themes honoring system preference, empty states that explain what a scope is and how inheritance works, and no information leakage in error pages (403 and 404 look the same for scopes the principal cannot see).
6. **Operational hooks:** health and readiness unaffected by portal failures, request IDs in logs, rate limiting per session, configurable base path so it can sit behind an ingress path or its own hostname, and Helm values added in stage 06 (list the config you need in the handoff).
7. **Tests:**
   - Hierarchy visibility through the portal with a three-level tree, a sibling team, and a sibling project, for readers, writers, maintainers, an org-only member, a cascading org admin, and a removed member (access disappears on the next request).
   - Stored XSS attempts in content, rationale, source_uri, and scope names are neutralized; CSP header present; CSRF rejection for every POST.
   - Session fixation, expired session, logout invalidation, and cross-user session reuse.
   - SSO linking: authenticate the existing owner and upstream identity; verify ownership and history remain attached to the same principal, conflicting links are rejected, matching email alone does not link, and CSRF/replayed callbacks fail.

   - 403/404 indistinguishable for hidden scopes and hidden memory IDs.
   - Browser end-to-end tests (Playwright or similar) for sign-in, navigate down the tree, see inherited memories, search, open history, approve a proposal, and see the audit event.
   - Promotion: approve-and-promote as a maintainer of both source and destination (one step), as a source-only maintainer (becomes a request in the parent queue, then is approved or rejected there), as a reader or writer (action absent and server-side rejected), with a stale revision (clear conflict message), and double-submit (idempotent); the child's view shows the promoted memory as inherited with no duplicate; audience-impact disclosure respects membership visibility; destination-only reviewers cannot access source history, and stale or revoked source consent blocks acceptance.
   - Overrides: drafting a link does not hide inherited guidance; approval of the exact revision/link activates it in that scope and below only. Updates reset approval and disable it; archival/supersession also disables it. The toggle reveals the overridden memory; siblings are unaffected.
   - Extend the stage 04 adversarial suite with the portal endpoints and report results.

## Excluded
Free-form creating or editing memories in the browser, member and scope administration UI beyond read-only views, analytics dashboards, real-time collaboration, notifications, re-parenting scopes, any separate frontend deployment.

## Deliverables
`internal/portal`, templates and static assets, `docs/decisions/portal.md`, portal sections in the threat model, browser end-to-end tests, extended adversarial suite, user documentation (`docs/portal.md` with screenshots only if generated from the real app), `docs/handoff/05-portal.md`.

## Acceptance criteria
- A person signed in as a project reader sees project memories plus clearly labeled inherited team and org memories, and no descendant memories anywhere; an org-only member sees only org memories; a cascading org admin sees everything below.
- No view, count, search result, error message, or autocomplete reveals the existence of a scope or memory the principal cannot read.
- A reviewer promotes an approved project memory to its team; a team-level reader (not a project member) can then read it, project members see it as inherited, and no duplicate remains at the project; without destination authority, the request lands in the team's queue.
- When an approved project revision and its approved override link override an inherited organization convention, project views show the project memory and collapse the org one, while an org-level view is unchanged.
- A maintainer can approve an agent-proposed memory in the portal and an MCP search then shows it `approved` with the approver; the audit log records it.
- XSS, CSRF, and session tests pass; the CSP is enforced in the browser tests.
- The portal works in solo mode (token sign-in) and with the Keycloak upstream from stage 03.
- All tests pass against real PostgreSQL, and any skipped check is listed in the handoff.

## Handoff
`docs/handoff/05-portal.md`: routes and config the Helm chart must expose (base path, session secret, cookie settings, CSP overrides), test results, known gaps, deferred features (editing, member admin, notifications), and anything stage 06 must measure (portal page latency under load).
