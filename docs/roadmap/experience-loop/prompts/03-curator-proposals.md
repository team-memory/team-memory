# 03: Curator proposals

## Objective

Build a scheduled curator. It reads new captured sessions in one scope at a time and proposes memories as **unverified** revisions in the existing review queue, each with citations back to the session. This turns raw experience into reviewable knowledge, while leaving the decision about what agents follow to people.

## Context

Team Memory is a self-hosted Go service (PostgreSQL 17, pgvector, forced RLS, scoped memories, approval only in the portal, an append-only audit log). Stage 02 added redacted, retention-limited session capture.

Stash's curator rewrites a wiki directly. Team Memory's curator only proposes; a human approves an exact revision in the portal. The LLM provider is pluggable: Ollama is the self-hosted default, and an Anthropic adapter is optional, off by default and enabled per scope by an operator (if stage 01 kept it in scope).

## Inputs

- `docs/handoff/experience-loop-01.md` and `docs/handoff/experience-loop-02.md`. If either is missing, stop and ask for it.
- The decision records for the curator provider and the curator output contract.
- `internal/embeddings/` (an Ollama client pattern and conflict similarity), `internal/jobs/`, `internal/memory/` (create and propose paths), `docs/decisions/trust-rules.md`, `docs/eval/`
- For the Anthropic adapter, load the Claude API reference before writing code, and use the model IDs stage 01 chose. The keys come from operator configuration only.

## Work

1. **Provider interface.** Put a small provider interface in its own package: generate structured output from a prompt, with a timeout and a token budget.
   - Implement the Ollama provider.
   - If stage 01 kept it, implement the Anthropic provider behind explicit configuration.
   - When the configuration is missing or invalid, the curator stops cleanly.
   - Readiness and metrics report the provider state.
2. **Curator worker.** Add a scheduled job for each scope where capture and curation are enabled.
   - It reads sessions newer than a stored watermark, using its own database role with the least privilege it needs and RLS still enforced.
   - It advances the watermark only through what it actually processed.
   - It skips idle scopes without calling the LLM.
   - It enforces the caps on proposals per run and on tokens or cost from stage 01.
3. **Prompting and safety.**
   - Frame transcript content as untrusted data, with a clear system instruction that it must not be followed.
   - Require structured output that is validated against a schema. Discard anything invalid and count it in metrics.
   - Never pass secrets or unredacted content.
   - Run the secret scanner again on the proposed text before saving it.
4. **Proposals.**
   - Create memories only of the allowed types, through the same domain path agents use, so they arrive unverified and attributed to a curator principal with a clear client attribution.
   - Each proposal carries a `rationale` and a `source_uri` that resolves to a session excerpt the reviewer is allowed to see.
   - When a proposal is similar to an existing memory, attach it as a suggested update or flag it as a likely duplicate rather than creating a new memory, following stage 01's rule.
   - The curator principal has no role that allows approval or supersession. Prove this with a test.
5. **Review experience.** In the portal review queue:
   - Show curator proposals grouped by run.
   - Show their citations and a duplicate warning.
   - Add bulk reject, but not bulk approve.
6. **Evaluation.** Add a synthetic evaluation set under `docs/eval/curator/`: sessions with known lessons, sessions with nothing to learn, prompt-injection attempts and duplicates. Add a command or test that reports precision of the proposals against expected lessons, citation validity, duplicate rate and injection resistance.
7. **Tests.**
   - Unit tests with a fake provider.
   - Integration tests for watermarks, caps and scope isolation (a curator run for one scope never reads another scope's sessions or writes proposals into it).
   - Adversarial tests: a transcript that tells the curator to approve, supersede or write to another scope; and a proposal containing secrets.

Excluded: skills, the CLI, and any automatic approval or confidence-based promotion.

## Deliverables

- The provider package, the curator worker, migrations (watermarks, run records), portal review changes, the evaluation set and runner, and tests.
- `docs/operations/curator.md`, covering enabling curation, provider configuration, data egress, caps, metrics and alerts. Example alerts go in `deploy/`.
- A `CHANGELOG.md` entry.
- `docs/handoff/experience-loop-03.md`.

## Acceptance criteria

- `make test`, `make test-security` and `make lint` pass.
- Using Ollama on the evaluation set, the curator meets the thresholds stage 01 set. Record the actual numbers.
- No curator-created revision is ever approved without a human portal action, and the curator principal is refused by `memory_approve` and `memory_supersede`.
- Injection samples produce no proposals outside the intended scope and no instruction-following side effects.
- With the Anthropic adapter disabled, there are no outbound requests to Anthropic. A test or a network policy shows this.

## Handoff

Record in `docs/handoff/experience-loop-03.md`:

- Evaluation results and the provider and model used.
- The caps in effect.
- Known failure modes.
- The citation URI format that stage 05 must render.
- Open issues.
