# Decision: scope creation through MCP

Status: accepted (2026-11-24)

## Context

Scopes could be created only with the operator CLI, which connects as the operator database role. A deployment whose scopes follow something that appears at run time (a chat channel, a repository, a customer project) had to either run an operator step for each one or keep everything in one broad scope. The service already had `CreateScopeAs`, which creates a scope as the calling principal under the rule in [rls.md](rls.md), but no caller.

## Decision

Add one MCP tool, `memory_scope_create(parent, level, slug, name?)`, backed by `EnsureScopeAs`.

- **Same rule, no migration.** The caller needs a cascading maintainer grant covering the parent. Go checks it and the existing `app_insert` policy on `scopes` enforces it again with `created_by = tm_principal()`. A sibling subtree, another organization and a non-cascading grant fail that one rule, and a root scope cannot be inserted because it has no parent to hold a grant on.
- **Existing scopes are returned.** If the scope exists at the requested level, the result is that scope with `created: false`, and nothing is audited. Agents run in parallel and scopes cannot be deleted, so a strict "already exists" error would make every caller handle a race it cannot avoid. A concurrent insert that loses on the unique path index is retried once and finds the winner's row. This reveals nothing: a caller that passes the rule can read every child of the parent. A scope that exists at another level is `invalid_argument`.
- **Not found before forbidden.** A parent the caller cannot read is `not_found`; a readable parent without the grant is `permission_denied`, as for the memory tools.
- **A separate rate limit.** Scope creation is limited per principal (6/min, burst 10) below the request limit, because a scope is permanent and each insert recomputes `scope_access` for the principals with a cascading grant above it.

## Not done

- **Writers cannot create scopes.** Lowering the rule would let every cascading writer change the tree in every deployment, and would need a new policy. A deployment that wants an agent to create scopes grants it maintainer with cascade on the subtree it owns. Through MCP that also allows `memory_supersede` and review requests; approval still needs a person in the portal, and service principals cannot sign in there.
- **No tool to list, rename or move scopes.** A caller learns a scope is missing from `not_found`.
- **Level order is not checked**, as with the CLI: the trigger checks the label against the organization's allowed levels and the depth against its maximum.

## Tests

`TestScopeCreationThroughMCP` (refusals from each position without the grant, validation, the creator's access, repeat and concurrent calls, depth limit), `TestScopeCreationIsRateLimited`, the tool added to the `TestHierarchyIsolationThroughMCP` probe, raw-SQL scope inserts in `TestCrossTenantRawSQL`, and `scope.create` events in `TestAuditTrail`.
