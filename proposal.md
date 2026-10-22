# Team Memory

**A self-hosted, permission-aware shared memory for developers and their AI coding agents, served over MCP.**

## One-sentence pitch

Team Memory is an open-source Go service that lets Claude Code, Codex, and any MCP-compatible client read and write one shared, versioned, access-controlled body of team knowledge, with a clear line between what an agent *proposed* and what a human *approved*.

## Who it is for

- **Primary:** small-to-mid engineering teams that use more than one AI coding agent and want those agents (and the people behind them) to share decisions, conventions, and lessons.
- **Also:** self-hosters who want durable agent memory without depending on a particular model provider or SaaS.
- **Launch audience for this proposal:** prospective users and contributors of an open-source (Apache 2.0) project.

## The problem

Agent sessions start cold. Decisions made in one developer's session ("we chose Calico over Cilium, and why") are invisible to the next developer's agent. Per-user memory files and chat history do not travel across people, repositories, or clients. Worse, when an agent *does* write notes, nothing distinguishes a guess from an official team standard, so one agent's invented convention can be treated as fact by every other agent.

**Current alternatives (not yet a market survey):** repository instructions (`CLAUDE.md`, `AGENTS.md`), wikis and ADRs, and client-specific memory features. Repository files can already be shared through git; the hypothesis is that Team Memory adds searchable cross-session knowledge, explicit review, and access controls across clients and scopes. Whether agents consult any source depends on client configuration and agent instructions. Stage 01 checks existing alternatives before implementation; stage 07 refreshes that comparison.

Accepted implementation clarifications are in `review-decisions.md`; attach it to every build stage. V1 establishes its initial owner through `team-memory init`; later SSO linking requires explicit authentication to both identities.

## Proposed solution

A modular-monolith Go service (API + background worker) on PostgreSQL with pgvector:

- **Versioned knowledge records**, not transcripts: five kinds (`fact`, `decision`, `convention`, `lesson`, `context`), immutable revisions, supersession instead of silent overwrite, optimistic concurrency.
- **Trust levels** (`unverified`, `cited`, `approved`). Only maintainers can approve through explicit browser review of an exact revision; MCP can request review but cannot finalize approval. `cited` means a citation was supplied, not verified. Retrieval always reports trust and provenance.
- **Hybrid search**: PostgreSQL full-text plus vector similarity, fused with Reciprocal Rank Fusion, permission-filtered, with full-text still working when embeddings are pending or the model server is down.
- **Real MCP** over Streamable HTTP (JSON-RPC, capability negotiation, tools, resources, OAuth discovery), not REST relabelled as MCP.
- **Hierarchical scopes**: knowledge lives in a tree (default levels: organization → team → project; admins can name additional levels, up to a configured depth). Memories flow **down** only: a project's context includes its team's and organization's memories, labeled with where they came from. Project memories never appear at team or organization level, and a team's never appear in the organization's. Knowledge is inherited at read time, never copied. The only way a memory moves up is an explicit, human-approved **promotion** one level at a time (below). An explicit override becomes effective only when its exact revision and override link are approved; then **the lowest (most specific) scope wins**. Unapproved proposed overrides show alongside inherited guidance and cannot hide it.
- **Web portal**: signed-in people browse and search the memories visible at any scope they can access, with inherited memories shown separately and labeled, plus history, provenance, and trust. Maintainers get a review queue to approve or reject agent-proposed memories, and a source maintainer can submit an approved revision for promotion up one level, with explicit destination-maintainer acceptance (it becomes a memory of the parent scope, with a link back to where it came from; the original is superseded by it, so nothing is duplicated). It uses the same login as MCP.
- **Solo to enterprise auth**: a one-person install needs no IdP (`init` plus a personal access token); a team adds any OIDC provider through Kubernetes Secrets, with domain allowlists and optional group-to-role mapping.
- **Security by layers**: individual identities, project roles, PostgreSQL row-level security, append-only audit log, treating all retrieved memory as untrusted input.
- **Provider-independent**: pluggable embeddings (local Ollama first), no dependency on any model vendor.
- **One-way git mirror**: PostgreSQL stays the source of truth. A worker job exports every memory revision as a markdown file (frontmatter plus content) to a private git repository, one commit per exportable lifecycle event, with event ID, content revision, author, trust level, and approval metadata in the commit. This gives browsable history, diffs, and supplementary off-site recovery material; PostgreSQL backups remain necessary. Rollback is still done through the service (a new revision or supersession), not by `git revert`. Git is not imported during normal operation in v1; an explicitly invoked disaster-recovery procedure may reconstruct limited knowledge content without restoring authoritative approval records. All git operations use the pure-Go [go-git](https://github.com/go-git/go-git) library, so the container needs no `git` binary.

### Example

Developer A, in Claude Code, records: *"Decision: use PostgreSQL for payments-api — ACID transactions"* with a link to the issue. It lands as `unverified` / `cited`. A maintainer reviews and approves the exact revision in the portal. Later, Developer B, in Codex, asks "why did we choose PostgreSQL?" and gets the decision, its rationale, source link, author, revision, and approval status.

## First-version scope

**In:** memory create/read/revise/supersede/archive with immutable revisions and lifecycle events; 8 MCP tools (`memory_search`, `memory_get`, `memory_create`, `memory_update`, `memory_supersede`, `memory_history`, `memory_approve`, `memory_recent`); MCP resources for scope overview/decisions/conventions/recent; hierarchical scopes with downward inheritance, human-approved promotion up one level, and lowest-scope-wins conflict resolution; a read-and-review web portal; hybrid retrieval; embedding worker with durable jobs; one-way git mirror of memory revisions to a private repo; OIDC auth, scope-level roles, RLS, audit log, approval workflow; Helm chart, backups, metrics; Claude Code and Codex integration; open-source release hygiene (license, README, contribution and security docs, example agent policy file).

**Out (deliberately):** git as the source of truth, normal-operation importing from git, PR-based approval (a possible later step; the mirror's file format is chosen to make it feasible), automatic extraction of every conversation, knowledge graph, Slack ingestion, agent-to-agent messaging, free-form memory editing in the portal (v1 portal is read, search, and review only), analytics dashboards. These can follow once the core is trustworthy.

## Milestone that defines success

Developer A records a decision in Claude Code; Developer B retrieves it in Codex; both see who created it, why, and whether it is approved — with tenant isolation and downward-only inheritance proven by adversarial tests. A third person signed in to the portal sees the same memory, and sees an organization-level convention inherited into the project but not the project's memories at organization level.

## Validation and rollout

Built in stages (see `prompts/00-start-here.md`): foundation, retrieval, identity-provider decision, team security, web portal, operational hardening, then client verification and open-source release. Each stage ends with checkable acceptance criteria. Initial rollout is the author using multiple machines on the same project, hosted on an existing Kubernetes cluster. Verify remembered decisions across machines and clients in actual project work. Separate test identities exercise isolation, approval, and role boundaries; one person using those identities is not evidence of multi-person adoption. A later multi-person pilot should evaluate whether teams find retrieval and review useful. Public release preparation follows cross-client and operational verification, with team-usability claims limited to evidence actually collected.

**Initial engineering targets (to be validated by testing, not promises):**

| Metric | Target |
| --- | --- |
| MCP availability | 99.9% monthly |
| Search latency | p95 < 500 ms warmed, excluding query embedding |
| Write latency | p95 < 300 ms, excluding async embedding |
| RPO / RTO | 15 min / 2 h |
| Cross-tenant data leaks | zero tolerated |

The private pilot records real examples of useful retrieval, missed/stale guidance, time spent reviewing proposals, and setup friction on each machine. These observations complement protocol tests; they do not establish team adoption. The pilot report may recommend changes without presenting unmeasured time savings as facts.

## Differentiation (hypotheses, not evidence)

- Trust/approval model that stops agents laundering guesses into policy.
- Client- and model-neutral via standard MCP.
- Permission-aware retrieval enforced in the database, not only in app code.
- Plain Postgres operations story (no separate vector database).

## Timeline

The author wants a GitHub launch as soon as stability is demonstrated. Required security, client compatibility, clean-install, backup/restore, and operational checks determine readiness; a calendar estimate does not. No pilot project is selected yet, and the author has no self-hosted SSO. Development uses disposable local Keycloak for auth tests, while solo operation supports personal access tokens without an IdP. Select a real pilot project before the pilot; synthetic test data is not adoption evidence.

The earlier four-week estimate is unvalidated and is not a deadline or a stage budget. Plan by completed milestones, retaining the agreed v1 scope. Re-estimate after foundation and authentication discovery expose integration costs; no measured delivery estimate is available yet.

## Risks, assumptions, open questions

- **Authentication architecture is a working assumption.** The goal is one person with no IdP, up to a large team bringing its own SSO. The plan is personal access tokens for solo/CI use plus a Team Memory OAuth layer that delegates login to any configured upstream OIDC provider (SAML via a broker). This adds security-sensitive code, so a maintained Go OAuth library is used rather than hand-rolling, and a discovery stage proves it with Claude Code and Codex before security work. IdP client IDs and secrets are supplied as Kubernetes Secrets before first sign-in, and the deployment guide documents this.
- **MCP/OAuth client compatibility** in Claude Code and Codex varies by version; must be tested, not assumed.
- **Search "score" calibration:** RRF scores are rank-based, not calibrated probabilities. The first version should define an honest, documented relevance score and evaluate it on a small labeled query set.
- **pgvector + selective tenant filters:** a global HNSW index can lose recall under filtering; benchmark and decide (partial indexes, iterative scan, partitioning).
- **Schema gaps in the draft:** revision and embedding tables lack tenant columns for RLS; org membership, audit, approvals, idempotency keys, and job tables are not yet designed.
- **Embedding model quality** (`nomic-embed-text` as initial candidate) on code and architecture vocabulary is unproven; changing models must not require rewriting knowledge records.
- **Prompt injection and secrets:** stored memories are an injection vector; secret scanning and untrusted-data framing are required, not optional.
- **Existing alternatives not yet surveyed:** stage 01 performs a focused comparison before implementation; stage 07 refreshes it before release. No demand or superiority claim is established yet.
- **Hierarchy semantics need care:** access and knowledge flow are different things. Reading a scope's memories requires membership there or a cascading grant from an ancestor; anyone who can read a scope also reads its ancestors' memories (inherited, read-only). Nothing flows up automatically; promotion is the one exception and is explicit, human, and audited. Conflict resolution is lowest-scope-wins, which means a project can override an inherited organization convention, including ones the organization may care about (for example security rules); a non-overridable ("locked") flag for org-level memories is a likely later need and is not in v1. Detecting that two memories conflict is hard to do automatically, so v1 uses explicit `overrides` links (proposed by writers or suggested by similarity in the review queue, effective only after revision-bound human approval) plus a precedence rule in results. Promotion widens who can read a memory, so it re-runs secret scanning and requires confirmation showing the new audience. Row-level security over a tree is easy to get wrong and needs dedicated adversarial tests.
- **Portal is a new attack surface:** browser sessions, CSRF, and rendering untrusted memory content (stored XSS) are real risks. Memories are rendered as sanitized text/markdown with a strict content security policy.
- **Git mirror exposure:** git permissions are per repository, not per row, so anyone with read access to the mirror sees everything in it. V1 mirror access is restricted to trusted deployment operators and the export worker. End users browse authorized history through Team Memory; scope membership never grants direct git access. Keep one repository per owning scope, with inherited memories not duplicated into descendant repositories. Operators are trusted with exported scope content; git copies do not provide application-level revocation. Secrets that reach history stay there until it is rewritten, so secret scanning must run before export. Archival and deletion semantics in the mirror need an explicit policy.
- **Hostnames and environment specifics** (e.g. a private domain) must stay out of the open-source code and docs.

## The ask

For the open-source launch: try the self-hosted release, test it with your agent of choice, report client-compatibility and security findings, and contribute. For the author: complete stages 01–07 in `prompts/` before any public announcement.
