# Decision: database roles and row-level security over the scope tree

Status: accepted (stage 04, 2026-10-21)

## Decision

1. **Three group roles**, created by migration `00003_security.sql` and joined by deployment login roles:

   | Role | Used by | Rights |
   | --- | --- | --- |
   | `team_memory_operator` | migrations, `init`, operator CLI | table owner (or a member of it): full DML; RLS bypassed by an `operator_all` policy on every table; guard triggers exempt it |
   | `team_memory_app` | `serve` | least-privilege DML, subject to forced RLS; no DELETE on history, no TRUNCATE, no DDL |
   | `team_memory_worker` | `worker` | reads the columns it embeds; writes embeddings, conflict candidates and jobs |

   `serve` and `worker` call `postgres.CheckRuntimeRole` at startup and refuse to run as a superuser, a role with `BYPASSRLS`, an operator, a table owner, or a member of the other runtime role. There is no override and no fallback to `DATABASE_URL`.

2. **Precomputed access** in `scope_access(principal_id, scope_id, org_id, rank, cascade_rank)`:
   - rank 1–3 is the effective role on a scope (direct membership, or a cascading grant on an ancestor);
   - rank 0 marks a scope readable only as inherited (an ancestor of a scope where the principal has a role);
   - `cascade_rank` is the strongest role held through grants that also cover every descendant.

   Row triggers on `scope_members` and `scopes` recompute one principal's rows (under a per-principal advisory lock) inside the transaction that changed the membership or created the scope. The functions that write `scope_access` are `SECURITY DEFINER` and not executable by the runtime roles.

   Every definer function and every guard trigger pins `search_path = pg_catalog, public, pg_temp`. `pg_temp` must be named, and last: when a path omits it, PostgreSQL searches it *first* for tables, and every role may create temporary tables by default, so a caller could shadow `idp_group_mappings`, `scope_closure` or `approvals` with a temporary table and forge grants, access rows or approvals (`TestFunctionsIgnoreTempTables`). Policies are not affected: their expressions are bound to table OIDs when created.

3. **Every tenant-owned table** has RLS enabled and **forced**. Policies are non-recursive, uncorrelated `scope_id IN (SELECT scope_id FROM scope_access WHERE principal_id = tm_principal() [AND rank >= n])` predicates. PostgreSQL evaluates each one once per statement as a hashed subplan. `TestEveryTenantTableHasForcedRLS` enumerates the schema, so a new table fails CI until it is classified.

4. **Request identity**: `postgres.WithActorTx` sets `app.principal_id` and `app.auth_method` with `set_config(..., true)` (transaction-local) at the start of every transaction. If the variable is unset, empty, or the nil UUID, `tm_principal()` is NULL and every policy matches nothing. A `TxGuard` query tracer on the app pool counts statements run outside a transaction. Every integration test asserts that count is zero.

## Rules as implemented in SQL

| Rule | Where |
| --- | --- |
| Own-scope reads need membership or a cascading ancestor grant | rank ≥ 1 rows exist only for those scopes |
| Ancestors are readable by anyone who can read a descendant | rank 0 rows |
| Descendants are never readable through an ancestor membership | no row unless a cascading grant reaches them |
| Writes need writer on the owning scope, never on an inherited one | `memories`/`memory_revisions` INSERT/UPDATE: rank ≥ 2 |
| Archive, supersede, review requests need maintainer | `tm_memories_before_update` guard; `memory_events` INSERT policy (rank ≥ 3 for those event types) |
| Approval needs a human maintainer in a browser review, bound to the current revision | `approvals` INSERT policy; guard allows `trust = approved` only with a matching approval |
| Override targets must be readable and owned by a strict ancestor | `revision_overrides` INSERT policy (closure join) |
| Creating a child scope or a cascading grant needs a cascading maintainer grant | `scopes`/`scope_members` policies on `cascade_rank` |
| The organization owner's root grant changes only through the operator | `tm_scope_members_before_change` |
| Promotion source supersession by a destination reviewer | `tm_promotion_finalize_source` (definer; rechecks the whole contract) |
| Scopes never move; memory scope and provenance never change | scope trigger (stage 01); memories guard |

The Go layer checks the same rules first and returns domain errors (`not_found`, `permission_denied`, `inherited_read_only`). RLS is the second layer. A Go bug yields an empty result or a refused statement (mapped to `permission_denied`), not a leak.

## Why a precomputed table rather than a STABLE function

The prompt asked for whichever is correct and fastest with deep trees and many members.

- **A `STABLE` SQL function** over `scope_members` ⋈ `scope_closure` evaluated inside policies fails on correctness first. `scope_members` and `scope_closure` need policies too, and a policy that calls a function reading its own table recurses ("infinite recursion detected in policy"). Breaking the recursion needs `SECURITY DEFINER`, which the prompt rules out for the visibility function, or unprotected base tables.
- **A precomputed table** has a policy that reads no other protected table (`principal_id = tm_principal()`), so every other policy can read it without recursion. Lookups are a primary-key range scan. The cost moves to writes: one principal's rows are recomputed per membership change. That is acceptable because memberships change rarely and reads dominate.

Measured on the local Docker PostgreSQL 17 (Apple Silicon), `TestDeepTreeManyMembers`: 16-level chain with 20 leaves per level (316 scopes), 2,000 principals, 4,000 grants, about 6,100 memories, 33,434 `scope_access` rows.

| Operation | Time |
| --- | --- |
| Full-text search from depth 15 (16 visible scopes), best of 5 | 3.3 ms |
| `count(*)` over all memories under RLS as a bulk member | 1.3 ms |
| Cascading reader grant on the root (316 scopes recomputed) | 4.7 ms |
| Its revocation | 1.3 ms |
| Bulk load of 4,000 grants (row triggers) | 1.9 s total |

Vector search under RLS: see "Vector index under RLS" below.

## Vector index under RLS

Measured with `make bench-vector` at 100,000 embeddings (the stage 02 dataset), shipped strategy (planner default, `relaxed_order`, `ef_search` 100), 20 queries per scenario. "plain" is the stage 02 path (superuser, no policies); "RLS" runs as `team_memory_app` with a principal holding a cascading reader grant on every organization, the worst case for `scope_access` size. Raw results: `docs/eval/vectorbench-100k-rls.json`.

| Scenario | Rows in filter | recall@50 plain / RLS | p50 ms plain / RLS | p95 ms plain / RLS |
| --- | --- | --- | --- | --- |
| org root (depth 0) | 2.00% | 1.000 / 1.000 | 3.8 / 15.1 | 4.4 / 15.9 |
| team (depth 1) | 2.40% | 1.000 / 1.000 | 4.9 / 15.6 | 5.9 / 16.7 |
| project (depth 2) | 2.80% | 1.000 / 1.000 | 5.5 / 16.4 | 7.1 / 17.1 |
| deep leaf (depth 8) | 7.40% | 1.000 / 1.000 | 10.4 / 24.1 | 12.1 / 25.0 |
| small-org project | 0.33% | 1.000 / 1.000 | 6.5 / 11.4 | 7.7 / 12.9 |
| unfiltered reference | 100% | 0.995 / 0.997 | 4.2 / 2.8 | 6.6 / 3.3 |

Recall is unchanged, and filtered queries still never come back short (50 of 50 rows). Latency of filtered queries rises 2–4×. The `memories` policy turns the plan into a hash join over a sequential scan of `memories` and `scope_access`, instead of the plain path's nested loop over the `(model, scope_id)` btree. p95 stays at or below 25 ms, far inside the 500 ms search target. It grows with table size, though, so stage 06's load test should re-measure at the target scale. If it becomes a problem, the first lever is a scope-filtered index on `memories (scope_id, status)` that the policy predicate can use. Measured on Docker Desktop (Apple Silicon), not the Kubernetes target.

## Consequences and residual risks

- **The app role sets its own identity.** SQL injected into the app process could call `set_config('app.principal_id', …)` and impersonate anyone; `TestIdentityVariableIsTheTrustBoundary` demonstrates it. RLS defends against Go authorization bugs and stray queries, not against arbitrary SQL execution in `serve`. The defenses against that are parameterized queries only (sqlc and fixed SQL), no dynamic SQL from input, and the role limits above (no DDL, no history deletion, no audit rewriting, no access-table writes, no bypass). `app.auth_method = portal` is likewise a guard against Go bugs, not proof of a browser.
- **Identity tables are not RLS-protected.** `principals`, `principal_identities`, `personal_access_tokens` (hashes only) and the `oauth_*` tables are global, protected by privileges. The app role can read principal display names and token hashes.
- **The operator role bypasses everything.** It is for migrations and the CLI only, and must never be the serve connection (enforced).
- **New tables** must be added to the RLS list and grants in a migration. The enumeration test enforces this.
- **Promotion** relies on one definer function. It is short, rechecks every condition itself, and is executable only by the app role. Tests cover a caller without destination authority.
- **Superusers** (the local dev `teammemory` role) bypass RLS. Local tests therefore connect code under test as `tm_test_app`/`tm_test_worker` login roles, created by `internal/testdb`.

## Operating it

- Create login roles: `team-memory db create-login --role app|worker|operator --user NAME` (needs `CREATEROLE`). Set the password with `TEAM_MEMORY_NEW_LOGIN_PASSWORD`, or let the command generate and print one.
- `DATABASE_URL` is the operator connection. `TEAM_MEMORY_APP_DATABASE_URL` is for serve and `TEAM_MEMORY_WORKER_DATABASE_URL` for worker.
- If the migrating role is not a superuser, it needs `CREATEROLE` to create the group roles, or an administrator pre-creates them. The migration tolerates existing roles. It grants `team_memory_operator` to the migrating role.
