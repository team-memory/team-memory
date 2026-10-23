# Handoff: stage 07 (partial)

> **Superseded on 2026-10-23:** the author reports the cross-client scenario passed, and the project is now a personal proof of concept (no external review or pilot planned). Current status: [release checklist](../release-checklist.md). The rest of this page is the original stage 07 record.

## Go / no-go
**No-go for publishing.** The cross-client scenario (`docs/verification/cross-client.md`) and private pilot (`docs/verification/private-pilot.md`) were not run; per the stage rules that is an incomplete gate, not a pass.

## Done in this pass
- Reviewed stages 01-06 against their prompts: all listed deliverables exist. `make test` (race, real PostgreSQL), `make lint`, `make helm-lint`, `make check-alerts`, `make secret-scan` and `govulncheck` all pass.
- Fixed a stale README status banner (it claimed stage 05 and no Helm/operations).
- Added CONTRIBUTING (with versioning policy), CODE_OF_CONDUCT, SECURITY (placeholder contact), CHANGELOG, NOTICE, issue/PR templates, `lint` + `govulncheck` CI job, `docs/clients/{claude-code,codex}.md`, `docs/verification/repo-audit.md`, `docs/release-checklist.md`.
- Agent policy files already existed (`docs/agent-policy/`); unchanged.

## Not done, and why
| Item | Reason |
| --- | --- |
| Cross-client scenario | needs a running deployment, distinct identities, interactive portal approval |
| Private pilot | author must pick a project; needs real use across two machines |
| Landscape refresh | needs cited web research; `docs/landscape.md` still stage 01 |
| Full README walkthrough (quickstart to connected client), CI on GitHub | not run; image build and fresh-clone build/vet were checked and pass |
| Client docs re-verified on Claude Code 2.1.304 | verified on 2.1.292 only; `NODE_EXTRA_CA_CERTS` tip is general advice, not tested |

## Manual steps for the user
Resolved by the author on 2026-10-22: security contact, module path (matches the repo URL), planning docs ship publicly (private paths and cluster references scrubbed), git author email is public.

Remaining:
1. Run the cross-client scenario, with Codex tested by the author (`make k3d-up`, see the stage 06 handoff).
2. Pilot: the author is open-sourcing this as a personal project; a real-project pilot is optional and can be recorded in `docs/verification/private-pilot.md` after launch if it is to be claimed at all.
3. Refresh `docs/landscape.md` with cited research.
4. Recommended: external security review. Then create the repo, push, enable GitHub private vulnerability reporting, tag `v0.x.y` (the release workflow publishes image and chart), announce. No deploy has been done.
