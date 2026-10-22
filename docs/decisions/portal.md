# Decision: web portal architecture

Status: accepted (stage 05, 2026-10-22). Implements `prompts/05-portal.md` under `review-decisions.md`.

## Decision

Server-rendered HTML with Go `html/template`, embedded in the server binary (`internal/portal`, `go:embed`). **No JavaScript at all.** One stylesheet. No Node build, no extra deployable. The portal is an adapter over `internal/memory` and `internal/search`, like the MCP adapter. It never reads tenant tables itself: every request runs the same service methods, in the signed-in principal's transaction, under row-level security. The only SQL in `internal/portal` is its own session tables.

## Alternatives

| Option | Why not |
| --- | --- |
| SPA (React or similar) on a JSON API | Needs a build chain, a second deployable or an embedded bundle, a new JSON API surface to secure and test, and inline-script or bundle CSP exceptions. Nothing in v1 (read, search, review) needs client-side state. |
| Server-rendered plus htmx | Considered, as the prompt's default. Every v1 interaction is a link or a form post that redirects, so htmx adds nothing but a script dependency (vendored file, CSP `script-src 'self'`). Left out; it can be added later without changing handlers. |
| Server-rendered with a little hand-written JS | Same cost with no benefit now. |

A concrete reason to prefer a SPA did not appear, so the default stands, without htmx.

## Consequences for the design

- **CSP is `default-src 'none'`** plus `style-src 'self'`, `img-src 'self'`, `font-src 'self'`, `form-action 'self'` and the upstream identity provider origins (browsers apply `form-action` to the redirect after a post), `frame-ancestors 'none'`, `base-uri 'none'`. No `unsafe-inline`, no nonces, no `script-src`. A reflected or stored injection has no script to run even if sanitization failed.
- **Untrusted content never reaches HTML as markup.** `render.go` escapes every character as it copies it and emits only a fixed set of elements. Links allow `https`, `http`, `mailto` and configured schemes, and always carry `rel="noopener noreferrer nofollow"`. Images render as a text placeholder, never loaded. `javascript:`, `data:`, `vbscript:` and `file:` can never be allowlisted.
- **Sessions are server-side** (`portal_sessions`, migration 00006), not signed cookies, so logout and revocation are real and nothing needs a shared signing secret. The cookie holds 256 random bits; the table holds only its SHA-256.
- **Forms with CSRF tokens.** Each session has a random CSRF token embedded in every form; state-changing requests also pass an `Origin`/`Sec-Fetch-Site` check. Sign-in forms use a pre-session token in a cookie.

## Sessions and sign-in

| Aspect | Rule |
| --- | --- |
| Cookie | `__Secure-tm_portal` when the public URL is https, else `tm_portal`; `HttpOnly`, `SameSite=Lax` (needed so the redirect back from the identity provider carries it), `Path` = base path. No tokens in URLs, no local storage. |
| Timeouts | Idle 30 min, absolute 12 h (configurable). `last_seen_at` moves at most once a minute. |
| Rotation | Every login issues a new random id and revokes the one the browser presented. A planted id is never adopted. |
| Logout | `POST` with CSRF; revokes the row. |
| Principal | Loaded per request; a disabled or removed principal ends the session. Roles are read per request (RLS), so removal takes effect on the next request. |
| Service principals | Cannot sign in to the portal. |
| Token sign-in | `TEAM_MEMORY_PORTAL_TOKEN_SIGNIN`: `auto` (default) allows it in solo mode only; with SSO configured nobody may sign in with a token, so the identity provider's policy (MFA, domain and group rules, deprovisioning) cannot be bypassed. `owner` also allows the organization owner while they have no linked SSO identity (how the owner reaches Connect SSO; it closes once they have linked one). `all` allows any human without a linked identity (discouraged). `off` never. Unknown values stop startup. |
| SSO sign-in | Same upstream providers, allow rules, principal mapping (`(issuer, subject)`, never email) and group sync as MCP sign-in. The portal uses the facade's single registered callback URL: states starting `p.` are handed to the portal. One redirect URI per provider. |
| Fail closed | SSO configured but invalid: `/portal/...` answers 503 like `/mcp` and `/oauth/...`. |
| Browser binding | The login state is single-use, 10 minutes, bound to the starting browser by a cookie hash. Replayed or cross-browser callbacks fail. |

## Connect SSO

The signed-in person (a portal session) posts `Connect <provider>`. The portal stores a `link` state bound to that session and the browser, and redirects to the provider with `prompt=login&max_age=0`. On the callback it requires the session to still be alive, exchanges the code, and calls `Identities.LinkFromPortal`, which requires a portal session and an ID token `auth_time` no older than 5 minutes. The identity attaches to **that** principal (ownership and history are untouched); an identity already linked to another principal is refused; email is never consulted. The login must also be newer than the moment Connect was started. If the provider returns no `auth_time`, linking fails with a clear message (the freshness cannot be proved). No browser bootstrap exists: the owner is still created only by `team-memory init`.

## What the portal can and cannot do (v1)

Read, search, history and diffs, provenance, review queue, approve, reject (note required), archive, supersede (creates an unverified replacement), promotion request and acceptance, Mark as overriding, members listing, Connect SSO. **Not** in v1: free-form create or edit, member or scope administration, analytics, notifications, re-parenting.

## Trusted review and what "browser review" means

The portal sets `AuthMethod = portal` only on principals loaded from a live cookie session after CSRF validation. That is the boundary stage 04's database policies check. It is a workflow boundary, not proof of a human; see `docs/security/threat-model.md`.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `TEAM_MEMORY_PORTAL` | `on` | `on` or `off`; other values stop startup |
| `TEAM_MEMORY_PORTAL_BASE_PATH` | `/portal` | mount path; `/` for the root of its own hostname |
| `TEAM_MEMORY_PUBLIC_URL` | none | origin for the `Origin` check and the `Secure` cookie flag; empty trusts `Host` and uses insecure cookies (local development only) |
| `TEAM_MEMORY_PORTAL_IDLE_TIMEOUT`, `..._ABSOLUTE_TIMEOUT` | `30m`, `12h` | session lifetimes |
| `TEAM_MEMORY_PORTAL_SOURCE_SCHEMES` | none | extra link schemes (`jira,github`) |
| `TEAM_MEMORY_PORTAL_TOKEN_SIGNIN` | `auto` | `auto`, `owner`, `all` or `off` (see above) |
| `TEAM_MEMORY_PORTAL_CSP_EXTRA` | none | extra CSP directives, appended |

No session secret is needed: sessions and CSRF tokens are random values in PostgreSQL.
