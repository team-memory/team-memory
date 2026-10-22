# Using the Team Memory portal

The portal is a web page served by `team-memory serve` at `/portal` (see `TEAM_MEMORY_PORTAL_BASE_PATH`). It needs no extra install.

## Signing in

- **Solo setup (no SSO configured).** Run `team-memory init`, which prints a personal access token. Open `/portal`, paste the token under *Personal access token*. The token is sent once and is not kept in the browser.
- **With single sign-on.** Choose *Sign in with <your provider>*. A new person is created on first sign-in **with no access**; a maintainer or administrator grants roles.
- **With SSO configured, token sign-in is closed by default.** An operator who needs the owner created by `init` to reach Connect SSO sets `TEAM_MEMORY_PORTAL_TOKEN_SIGNIN=owner`.
- **Connecting SSO to an existing account** (for example the owner created by `init`): sign in with the token (in `owner` mode), open *Account*, choose *Connect <provider>*. You sign in again at your identity provider so both identities are proven now. The identity attaches to your account; ownership and history stay put. An identity already linked to someone else is refused, and email addresses are never used to match accounts. Afterwards, sign in with SSO; token sign-in is closed for that account (the token still works for MCP).

Sessions end after 30 idle minutes or 12 hours. *Sign out* ends them on the server.

## Reading

- **Scopes** lists the scopes you can browse into. Names of scopes you cannot read never appear.
- A scope page has two tabs. **This scope's memories** are the ones written here. **Inherited from ancestors** are read-only memories from the scopes above, grouped by where they come from. Nothing from the scopes below is ever shown.
- Badges: *Approved* means a maintainer reviewed that exact revision. *Cited* means a source link was given, **not** that anyone checked it. *Unverified* means nobody has vouched. Anything not approved carries a notice that it is not team policy.
- **Overrides.** Where a lower scope has an approved memory that overrides an inherited one, the inherited memory is collapsed ("Overridden at …: lowest scope wins"). *Show overridden* reveals it.
- **Search** uses the same hybrid retrieval as MCP and shows the same provenance.
- A **memory page** shows content, rationale, source, author, approver, the current revision, every revision with a line diff, lifecycle events and the supersession chain.

## Reviewing (maintainers)

Open *Review queue* on a scope (or a memory from it). The page names the exact revision you are reviewing, shows a secret-scan result and similar memories in other scopes.

- **Approve** binds to that revision. If it changed while you looked, you get a message and nothing is applied.
- **Reject** needs a note; the memory stays visible, unapproved.
- **Archive** hides it from default views. **Supersede** creates a replacement (unverified) in the same scope.
- **Promote** moves an approved memory up one level. You consent to share its snapshot with the parent scope's reviewers. If you are also a maintainer there, it completes at once; otherwise it becomes a request in the parent's queue, where a maintainer there accepts or rejects it. Destination reviewers see only the snapshot. Accepted promotions supersede the original, so there is no duplicate.
- **Mark as overriding** (writers) appears next to a similar inherited memory. It creates a new revision; nothing is hidden until a maintainer approves that revision.

Changing roles stays in the CLI (`team-memory member …`); *Members* is a read-only list.
