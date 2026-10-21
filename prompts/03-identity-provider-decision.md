# Stage 03: Identity Provider Decision (discovery; must finish before stage 04)

## Objective
Decide and prove the authentication architecture so Team Memory works for one person with no IdP and for a large team bringing its own SSO. Working assumption (the author's recommendation; confirm or overturn it with evidence): Team Memory acts as a thin OAuth authorization server for MCP clients and delegates login to a customer-configured upstream OIDC provider, issuing its own short-lived tokens with Team Memory as the audience. Personal access tokens cover solo use, CI, and service accounts. Prove it with working flows in Claude Code and/or Codex against a throwaway test deployment.

## Context
Team Memory is an open-source Go service exposing MCP over Streamable HTTP; clients include Claude Code and Codex (see `proposal.md`). Requirements: individual user identities (OIDC/OAuth 2.1 with PKCE), per-user project roles, no reliance on network membership (the author uses a private Tailscale network but authorization must hold regardless). There is no single chosen IdP: the deployment is configured with any OIDC provider (Google, Entra, Okta, Keycloak, Authentik, others). SAML is supported only via a broker (e.g. Keycloak or Dex) in front, documented not built. Deployment is on Kubernetes, where IdP client IDs and secrets are supplied as Kubernetes Secrets and configured **before** any user signs in; the server must fail readiness with a clear message when SSO is enabled but its secret or config is missing or invalid, and must not start an unauthenticated fallback. Why a facade is the working assumption: many enterprise IdPs lack dynamic client registration or the audience handling MCP clients expect, so pointing clients straight at them works for some IdPs and fails for others. Keycloak and Authentik are useful as test upstreams, not as required choices.

## Inputs
- `prompts/00-start-here.md` (attach; follow its global rules and completion gates).
- `review-decisions.md` (attach with the proposal; required accepted security/lifecycle decisions).
- `proposal.md`, `docs/handoff/01-foundation.md` (attach).
- The repository root (`/Users/tyler/Documents/projects/memory/team-memory/`), which holds both the service code and these planning files.
- The author has no self-hosted SSO deployed. Use the already-planned disposable local Keycloak setup for discovery; this is not a production IdP selection. Solo PAT mode does not require an IdP. Do not require the author to deploy SSO before development or assume access to a hosted provider.

## Work
1. Research the **current** MCP authorization requirements from the official specification: protected-resource metadata and authorization-server discovery, token audience (resource indicator) validation, PKCE, and how clients register (dynamic client registration, client ID metadata documents, or pre-registered clients). Cite the spec version and URL in the decision record.
2. Check, with the client versions the user has installed (ask for `claude --version` and Codex version), how each client performs OAuth for HTTP MCP servers, and what it requires from the authorization server. Distinguish what you verified by running it from what you read in docs.
3. Compare the two architectures against the verified requirements: (A) facade authorization server with upstream OIDC, versus (B) resource-server-only validating tokens issued directly by the customer's IdP. Criteria: works with IdPs lacking dynamic client registration, audience handling, client compatibility, solo-mode simplicity, security-sensitive code we must own, operational weight. Also choose the Go OAuth/OIDC library for (A) (maintained, audited-by-use, license compatible; do not hand-roll the protocol) and record alternatives. Use a small weighted criteria table; show reasoning.
4. Define the three auth modes behind the `Authenticator` interface and how they coexist: personal access tokens (hashed at rest, scoped, expiring, revocable, usable as a bearer header), upstream OIDC login, and optional direct-validation mode if (B) proves worthwhile. Define solo bootstrap: first run creates the org, default project, and an admin token via `team-memory init`, with no IdP required, and the path to later add an OIDC provider and link the existing user to their SSO identity.
5. Stand up test upstreams locally with docker compose in `deploy/dev-idp/` (throwaway, dev-only credentials, not committed as secrets): Keycloak plus one IdP that does not support dynamic client registration, or a documented emulation of that limitation. Prototype the login flow in Go behind `internal/auth`: metadata endpoints (`/.well-known/oauth-protected-resource`, authorization server metadata), PKCE, client registration approach, upstream code exchange, issuance of Team Memory tokens, JWKS, issuer/audience/expiry validation. Keep it behind a build/config flag if stage 04 will finalize it.
5a. Specify the Kubernetes configuration contract: which values and Secret keys are needed per upstream provider (issuer URL, client ID, client secret, allowed email domains or groups, token signing key), how multiple issuers per org are declared, rotation of the signing key and client secret without downtime, and the readiness behavior when any of it is missing or invalid. Stage 06 turns this into Helm values and the deployment guide.
5b. Implement the accepted v1 bootstrap decision: `team-memory init` establishes the initial owner through operator CLI access and must not reset ownership on repeat runs. Do not reopen the choice or implement first-login ownership, browser setup tokens, or preconfigured SSO-owner bootstrap. Specify explicit portal SSO linking that proves control of both the existing account and the upstream identity, rejects identities already linked elsewhere, and preserves ownership. Document an operator recovery procedure; email equality alone never links accounts. Record these decisions and tests in the identity decision.
6. Define how a token maps to a `principal` (`issuer` + `subject`) and how project roles are assigned (database-managed membership vs. IdP groups); recommend one and explain tradeoffs.
7. Write `docs/decisions/identity-provider.md` with: decision, criteria, evidence (what was run vs. read), what remains unverified, and consequences for stage 04.

## Excluded
Production deployment of the IdP, user provisioning for real people, RLS, approval workflow.

## Deliverables
`docs/decisions/identity-provider.md`; `deploy/dev-idp/` test setup; prototype token validation with tests; `docs/handoff/03-identity.md`.

## Acceptance criteria
- Decision record cites the spec version and distinguishes verified from assumed claims.
- A real MCP client (at least one of Claude Code or Codex) completes OAuth discovery and login against the dev stack and calls an authenticated tool using the selected architecture. Record failures and what was learned, but failed discovery/login does not complete this gate or authorize proceeding to stage 04 as though auth were proven. Full verification of both clients remains required in stage 07.
- Requests with a wrong audience, expired token, or wrong issuer are rejected (tests).
- The user can state, from the record alone, which architecture was chosen, which library implements it, why, and how an admin configures a new upstream IdP via Kubernetes Secrets.
- Solo bootstrap works end to end with no IdP: `init`, then a client connects with a personal access token.
- With SSO enabled but a missing or invalid secret, readiness fails with a clear error and no unauthenticated access is possible (test).

## Handoff
`docs/handoff/03-identity.md`: chosen architecture, library, and the Kubernetes config/Secret contract, client-specific quirks found, unresolved items, and the exact token-to-principal mapping stage 04 should implement.
