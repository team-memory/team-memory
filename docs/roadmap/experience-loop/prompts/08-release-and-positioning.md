# 08: Release and positioning

## Objective

Release the experience loop as a minor version, with documentation that lets an open-source user decide in a few minutes whether Team Memory fits them. Position it honestly against Stash and the other projects in `docs/landscape.md`.

## Context

Team Memory is a self-hosted, permission-aware shared memory for coding agents over MCP. It is positioned as a personal proof of concept with no external security review. This release adds session capture, curator proposals, skills, CLI and wiki views, and standards-api sync, all under forced RLS, scope inheritance and approval only in the portal. The pitch is in `docs/roadmap/experience-loop/proposal.md`. The audience is open-source users.

## Inputs

- `docs/handoff/experience-loop-01.md` through `-07.md`. Stage 07 must not list any open release blockers. If it does, stop and ask.
- `README.md`, `docs/landscape.md`, `docs/test-drive.md`, `docs/release-checklist.md`, `CHANGELOG.md`, and the release workflow in `.github/`

## Work

1. **README.** Add the experience loop to "What it does" and the documentation map. Keep the proof-of-concept status notice unless the author says otherwise. Keep setup steps accurate, and test each one against a fresh local stack.
2. **Test drive.** Extend `docs/test-drive.md` and the screenshot fixtures with the synthetic payments example from the proposal: capture, a curator proposal, approval, a skill, and a standards-api rule. Regenerate screenshots with `make docs-screenshots`.
3. **Comparison page.** Write `docs/comparison.md`. Cover Team Memory against Stash, plain repository files and the other projects in the landscape table. Include:
   - Where each one is the better choice.
   - What Team Memory deliberately leaves out: reward models, a public catalog, hosted SaaS.
   - Claims sourced from public documentation, each with the date it was checked.
4. **Proposal status.** Mark `docs/roadmap/experience-loop/proposal.md` as delivered. Note what changed from the plan and link the handoff notes.
5. **Release preparation.** Move the Unreleased changelog entries under the new version. Run the release checklist. Prepare the tag and release notes, but do not push a tag, publish a release or post anywhere without the author's explicit approval.
6. **Feedback path.** Add issue templates or a discussion prompt that asks adopters the questions from the proposal's "ask".

## Deliverables

- README, test drive, screenshots, `docs/comparison.md`, proposal status, changelog, release notes draft and issue templates.
- `docs/handoff/experience-loop-08.md`.

## Acceptance criteria

- Every claim about another project in `docs/comparison.md` links to a source and gives the date it was checked.
- The README quickstart and the test drive work on a fresh clone. Record the commands you ran.
- `make test`, `make lint`, `make helm-lint` and `make test-e2e` pass on the release commit.
- The release has not been published until the author approves it.

## Handoff

Record in `docs/handoff/experience-loop-08.md`:

- The release version.
- What is ready to publish.
- Deferred items carried into a backlog, such as reward models, source imports and a public catalog.
- The feedback questions to watch.
