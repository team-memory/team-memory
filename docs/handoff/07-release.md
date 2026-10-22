# Handoff: stage 07 (partial)

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
1. Set the security/conduct contact in `SECURITY.md`; confirm the author email in git history is acceptable.
2. Pick the real GitHub path and change the Go module path.
3. Decide whether `prompts/`, `proposal.md`, `review*.md` ship.
4. Choose the pilot project; run the cross-client scenario (`make k3d-up`, see stage 06 handoff).
5. Recommended: external security review. Then create the repo, tag, announce (all excluded here).
