# Cross-client verification (stage 07)

**Status: NOT RUN. This gate is incomplete, so the release assessment is no-go.**

The required scenario (Claude Code writes, Codex retrieves, and the reverse; approval in the portal; update; supersede; unauthorized user; hierarchy, promotion and override; mirror commits for each mutation) needs a running deployment with distinct authorized identities (writer, reader, maintainer, unauthorized) and an interactive browser for portal approval. That run did not happen in this session, and nothing here is claimed as observed.

## What exists as evidence (earlier, narrower)

| Evidence | Covers | Does not cover |
| --- | --- | --- |
| `early-clients.md` (stage 01, PAT, both clients) | create, search, get across both clients in one scope | approval, hierarchy, mirror, OAuth |
| `stage-03-oauth.md` (OAuth, both clients, Keycloak and Dex) | discovery, login, authenticated tools, refresh | the A-to-B scenario, approval, promotion |
| `integration/security/` suites (server-side, simulated clients) | trust rules, promotion, overrides, RLS, audit, mirror export | real client behavior |

## How to run it

`make k3d-up` and the `deploy/dev-k3d/` scripts (`setup.sh`, `fixtures.yaml`, `verify-fixtures.sh`, `client-env.sh`) provide a disposable cluster with Keycloak users. Follow `docs/handoff/06-operations.md` for tokens, then perform steps a-g and f2 from `prompts/07-client-verification-and-release.md` in both directions and record exact commands, versions and outputs here.
