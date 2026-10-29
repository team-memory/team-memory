# Team Memory Experience Loop

**One sentence:** Let coding agents learn from their own sessions the way [Stash](https://github.com/Fergana-Labs/stash) does, while every lesson still passes a human reviewer, stays inside your own infrastructure, and respects the permissions your organization already has.

Status: proposal, 2026-10-27. Nothing here is built. Claims about other projects come from their public repositories and were not independently tested.

## Who it is for

Platform and developer-experience teams that run AI coding agents (Claude Code, Codex and other MCP clients) across several teams. They want agents to stop repeating mistakes, and they also have to answer three questions:

1. Who decided this guidance, and when?
2. Can a project team see guidance meant only for another team?
3. Where did our agents' transcripts go?

## The problem today

Agents generate useful experience every session: corrections, dead ends and approaches that worked. Most of it is lost when the session ends.

| Option | What it gives you | What it lacks for this audience |
| --- | --- | --- |
| `AGENTS.md` / `CLAUDE.md` in git | Versioning and PR review | Someone has to write the lesson down; access is per repository; no retrieval |
| Team Memory 0.1 | Scoped, RLS-enforced, human-approved memories over MCP | No capture. Every memory is written on purpose, so most experience is never recorded |
| Stash | Automatic session capture, a curator that writes a wiki, auto-updating `SKILL.md` skills, CLI/SDK/VFS access | Account- or email-domain-scoped workspaces; privacy enforced in application code; no step where a human approves knowledge before agents follow it; hosted first |

Stash's loop of capture, curate and reuse is the most compelling part of the category. Team Memory's governance is the part enterprises ask about. No project we reviewed offers both.

## What we would build

The experience loop adds four capabilities on top of the existing memory model:

1. **Session capture.** Client hooks upload transcripts to a scope the user can already write to. Before storage, the server redacts secrets and applies the scope's retention policy. Capture is off until an owner enables it for that scope.
2. **Curator proposals.** A scheduled curator reads new sessions in one scope at a time. It proposes memories (lessons, conventions, decisions) as **unverified** revisions in the existing review queue. Each proposal cites the session excerpts it came from. The curator can never approve anything. The LLM provider is pluggable: local Ollama by default, with an optional Claude API adapter that operators enable explicitly.
3. **Skills.** A maintainer bundles approved memories into a versioned `SKILL.md` skill tied to exact approved revisions. Agents install skills with the CLI, and a session-start hook updates them. Only approved revisions ever reach an agent.
4. **CLI and wiki views.** A client CLI with a read-only, path-style browser (`/acme/platform/conventions/...`), plus portal wiki pages where memories link to each other and to the sessions that produced them.

A separate integration connects Team Memory to **standards-api**, a TypeScript service that serves versioned, severity-tagged engineering standards to AI code-review tooling. Approved Team Memory conventions can become standards, or standards can feed Team Memory. Which direction is the first decision to make. Each project keeps its own job: Team Memory holds and governs knowledge, and standards-api serves review-time rules filtered by repository, language and changed paths.

### Example

1. A Codex session in `acme/payments` proposes retrying card captures on every 5xx. The developer corrects it: only idempotent captures may be retried.
2. The capture hook uploads the session to `acme/payments` with the Stripe key in the log redacted.
3. That night the curator proposes a `convention`: "Retry card captures only with an idempotency key." It cites the turn where the correction happened.
4. A payments maintainer edits the wording in the portal and approves that exact revision. The audit log records who approved it, which client proposed it, and which session it came from.
5. The maintainer adds it to the `payments-safety` skill. The next morning every payments developer's agent picks up the updated skill at session start, and standards-api publishes it as a `high`-severity `reliability` rule for code review.
6. A developer in `acme/marketing` never sees the session, the proposal or the skill.

## Why this could matter (hypotheses, not evidence)

- **Capture raises recall.** We expect captured sessions to surface lessons people would not write down. *Test:* count curator proposals approved per week compared with memories written by hand.
- **Approval keeps quality up.** We expect human approval to stop prompt-injected or wrong lessons from spreading. *Test:* rejection rate and the reasons given, plus adversarial curator tests.
- **Governance is the differentiator.** We expect forced RLS, scope inheritance and revision-bound approval to matter to enterprise adopters. *Test:* issues, discussions and adoption feedback after release. We have no demand data today.

## First version

In scope:

- Decision records for capture data, retention and redaction, the curator provider, the skill format, the CLI surface, and the standards-api contract.
- Session ingest with forced RLS, per-scope opt-in, redaction, retention purge, audit events, and hooks for Claude Code and Codex.
- A curator worker with a provider interface, an Ollama adapter, an optional Anthropic adapter, citations, deduplication against existing memories, cost and rate caps, and an evaluation set.
- Skills built from approved revisions, with portal composition, CLI install and sync, and integrity checksums.
- A client CLI and read-only path browser, plus portal wiki pages with memory links and session citations.
- standards-api integration in the chosen direction, with idempotent sync and drift reporting.
- An updated threat model, a data-handling guide, Helm values for the new workers, load tests on ingest, and an honest comparison page.

Out of scope for now:

- Reward models, GEPA and prompt optimization.
- A public skill catalog, public publishing or forking.
- Hosted SaaS, a desktop app, a browser extension.
- Importing from GitHub, Google Drive, Notion or other sources.
- Auto-approval of any kind.

## Rollout and validation

1. Decide (stage 01) before writing code. Several choices, such as transcript retention and where the curator sends data, are hard to reverse.
2. Build capture and the curator behind feature flags. Dogfood them on this repository's own agent sessions using synthetic or personal data only.
3. Release skills and the CLI. Use the skills to deliver this repository's own agent policy.
4. Integrate standards-api. Check that one convention travels end to end, from proposal to code-review rule.
5. Harden, document and release as a minor version, and invite feedback.

**Success measures for the first version:**

- Adversarial tests show no data crossing scopes in sessions, proposals, skills or the CLI.
- No secret pattern from the existing scanner survives ingest in the test corpus.
- On the evaluation set, the curator produces proposals with working citations, and duplicate proposals stay under a threshold set in stage 01.
- A skill built from approved revisions installs and updates with both Claude Code and Codex.
- One approved convention reaches standards-api and comes back from `applicable_standards` for a matching changed path.

## Risks and open questions

- **Transcripts are the most sensitive data the system would hold.** They can contain secrets, customer data and proprietary code, and current project policy forbids storing secrets or customer data. Redaction reduces the risk without removing it. Capture must stay opt-in per scope, with a short default retention and an operator-run purge.
- **Curator prompt injection.** A malicious transcript could try to plant guidance. Human approval is the main defense, so the curator's output must always arrive as unverified proposals framed as untrusted data, never as instructions.
- **Data leaving the deployment.** The Anthropic adapter sends redacted transcript excerpts outside the deployment. It must be off by default and need operator configuration for each scope.
- **Review fatigue.** If the curator floods the queue, people will approve without reading. Caps, deduplication and batching decide whether the loop works at all.
- **Scope creep.** Copying Stash feature for feature would weaken what makes Team Memory distinct. Anything that needs public sharing or auto-approval is out of scope.
- **Open:** Which way should standards-api sync run, and should standards-api replace its single write key with per-client credentials first? Decided in stage 01.
- **Open:** Is session capture worth its risk for teams that already write memories by hand? Watched during dogfooding.

## The ask

For open-source readers: tell us whether a governed experience loop would solve a problem you have. Open an issue describing how your team handles agent-learned guidance today, and what would stop you from storing agent transcripts. Implementation starts with the stage 01 decisions in [`prompts/01-discovery-and-decisions.md`](prompts/01-discovery-and-decisions.md).
