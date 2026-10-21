# Decision: scope tree representation

Status: accepted (stage 01, 2026-10-20)

## Decision

Represent the tree as an adjacency list (`scopes.parent_id`) plus a **closure table** (`scope_closure(ancestor_id, descendant_id, distance)`), with a denormalized slug `path` (for example `acme/platform/payments`) kept for display and lookup.

Triggers maintain the structure:

- `tm_scopes_before_write` derives `org_id`, `depth` and `path` from the parent, enforces the organization's `max_depth` and `allowed_levels`, allows a missing parent only for `organization`-level roots, and rejects any later change to parent, org, slug, depth or path. Scopes cannot move in v1.
- `tm_scopes_after_insert` writes the closure rows (self at distance 0, plus one row per ancestor).
- Scopes and closure rows cannot be deleted.

## Why a closure table

The access rules need two queries that must be cheap, index-only, and easy to express inside RLS policies in stage 04:

1. **Roles that apply to scope S**: memberships on S, or `cascade = true` memberships on any ancestor of S. This is one join on `scope_closure` with `descendant_id = S`.
2. **Visible scopes from S**: S and its ancestors (`scope_closure WHERE descendant_id = S`). Descendants are never included.

Materialized paths (`ltree` or text prefixes) would also work, but prefix matching inside RLS predicates is harder to index and to reason about. Recursive CTEs over `parent_id` alone would be evaluated on every row check. The closure table has at most `depth` rows per scope (max depth defaults to 5), and it is written once because scopes do not move.

## Semantics implemented in stage 01 (service level)

- **Read a scope's own memories:** membership on the scope, or a cascading grant on an ancestor. A non-cascading grant gives access to that scope only.
- **Inheritance:** anyone who can read a scope also reads its ancestors' memories. Results carry `origin_scope` and `inherited`.
- **Nothing flows up:** reading a scope never reveals descendant memories, even for principals with a cascading grant.
- **Writing, superseding and archiving** need the matching role on the owning scope (directly or by cascade). Changing an inherited memory from a descendant scope returns `inherited_read_only`.
- **Not found vs forbidden:** scopes and memories a principal cannot read are reported as not found, so their existence is not revealed. Forbidden is returned only when the target is visible but the role is insufficient.

`max_depth` counts levels, so the default of 5 allows depths 0 to 4. Changing settings is refused if existing scopes would violate them.

## Deferred

- Moving scopes (would require rewriting closure rows and re-evaluating grants).
- RLS policies over these tables (stage 04). The tables already carry `org_id` and `scope_id` on every tenant-owned row, including `memory_revisions` and `memory_embeddings`, so the policies need no joins.
