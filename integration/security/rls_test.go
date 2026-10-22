package security

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/search"
)

// nonTenantTables are global identity and infrastructure tables. Every
// other table must have RLS enabled and forced. A new table fails this test
// until it is classified.
var nonTenantTables = []string{
	"goose_db_version", "principals", "principal_identities", "personal_access_tokens", "embedding_models",
	"oauth_clients", "oauth_auth_requests", "oauth_codes", "oauth_refresh_tokens", "oauth_pending_logins",
	"portal_sessions", "portal_login_states", "mirror_settings",
}

// TestEveryTenantTableHasForcedRLS enumerates every table in the schema.
func TestEveryTenantTableHasForcedRLS(t *testing.T) {
	w := newWorld(t)
	rows, err := w.db.Admin.Query(context.Background(), `
SELECT c.relname, c.relrowsecurity, c.relforcerowsecurity,
       EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname IN ('org_id', 'scope_id', 'principal_id') AND NOT a.attisdropped)
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
ORDER BY c.relname`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var name string
		var rls, force, tenantCols bool
		if err := rows.Scan(&name, &rls, &force, &tenantCols); err != nil {
			t.Fatal(err)
		}
		seen++
		if slices.Contains(nonTenantTables, name) {
			if tenantCols && name != "principal_identities" && name != "personal_access_tokens" && name != "portal_sessions" {
				t.Errorf("%s is listed as non-tenant but has tenant columns", name)
			}
			continue
		}
		if !rls || !force {
			t.Errorf("table %s: row security enabled=%v forced=%v; every tenant table needs both", name, rls, force)
		}
	}
	if seen < 25 {
		t.Fatalf("only %d tables enumerated", seen)
	}
}

// TestRuntimeRolesAreNotPrivileged: the app and worker roles cannot bypass
// RLS or own tables, and serve/worker refuse a role that could.
func TestRuntimeRolesAreNotPrivileged(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if err := postgres.CheckRuntimeRole(ctx, w.db.App, postgres.RoleApp); err != nil {
		t.Fatalf("app role: %v", err)
	}
	if err := postgres.CheckRuntimeRole(ctx, w.db.Worker, postgres.RoleWorker); err != nil {
		t.Fatalf("worker role: %v", err)
	}
	for name, pool := range map[string]*pgxpool.Pool{"superuser": w.db.Admin, "operator": w.db.Operator} {
		if err := postgres.CheckRuntimeRole(ctx, pool, postgres.RoleApp); err == nil {
			t.Fatalf("%s accepted as the app role", name)
		}
	}
	if err := postgres.CheckRuntimeRole(ctx, w.db.Worker, postgres.RoleApp); err == nil {
		t.Fatal("worker role accepted as the app role")
	}
}

// TestMissingIdentitySeesNothing: with app.principal_id unset (or empty, or
// the nil UUID), every tenant table is empty to the app role, inside and
// outside a transaction.
func TestMissingIdentitySeesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tables := []string{"scopes", "scope_closure", "org_settings", "scope_members", "scope_access", "memories",
		"memory_revisions", "revision_overrides", "memory_events", "approvals", "promotion_requests",
		"memory_embeddings", "conflict_candidates", "audit_events", "idempotency_keys"}
	for _, id := range []string{"", "00000000-0000-0000-0000-000000000000"} {
		tx, err := w.db.App.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if id != "" {
			if _, err := tx.Exec(ctx, "SELECT set_config('app.principal_id', $1, true)", id); err != nil {
				t.Fatal(err)
			}
		}
		var none bool
		if err := tx.QueryRow(ctx, "SELECT tm_principal() IS NULL").Scan(&none); err != nil || !none {
			t.Errorf("principal %q: tm_principal() is not NULL (%v)", id, err)
		}
		for _, table := range tables {
			var n int
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
				t.Fatalf("%s: %v", table, err)
			}
			if n != 0 {
				t.Errorf("principal %q sees %d rows of %s", id, n, table)
			}
		}
		_ = tx.Rollback(ctx)
	}
	// Outside a transaction (the TxGuard counts this; reset it after).
	var n int
	if err := w.db.App.QueryRow(ctx, "SELECT count(*) FROM memories").Scan(&n); err != nil || n != 0 {
		t.Fatalf("memories outside a transaction: %d %v", n, err)
	}
	w.db.Guard.Reset()
}

// TestCrossTenantRawSQL: an attacker running arbitrary SQL as the app role
// under a fixed identity cannot read another scope's rows (including
// embeddings, audit, history and revisions), cannot widen its access, and
// cannot alter history, audit or trust.
func TestCrossTenantRawSQL(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	// Give payments embeddings and audit rows.
	ix, err := embeddings.NewIndexer(ctx, w.db.Worker, embeddings.NewFake("nomic-embed-text", 768))
	if err != nil {
		t.Fatal(err)
	}
	wk := ix.Worker(jobs.Config{Name: "t"})
	for {
		n, err := wk.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	pay := w.mem["acme/platform/payments"]
	attacker := w.billingWriter
	for _, q := range []string{
		"SELECT count(*) FROM memories WHERE id = $1",
		"SELECT count(*) FROM memory_revisions WHERE memory_id = $1",
		"SELECT count(*) FROM memory_events WHERE memory_id = $1",
		"SELECT count(*) FROM memory_embeddings WHERE memory_id = $1",
		"SELECT count(*) FROM audit_events WHERE target_id = $1::text",
		"SELECT count(*) FROM jobs WHERE dedupe_key LIKE '%' || $1::text || '%'",
	} {
		if n := w.count(attacker, q, pay.ID); n != 0 {
			t.Errorf("%q: billing writer sees %d payments rows", q, n)
		}
		if n := w.count(w.paymentsMaint, q, pay.ID); n == 0 && !strings.Contains(q, "audit") {
			t.Errorf("%q: control: payments maintainer sees nothing", q)
		}
	}
	// Vector search through raw SQL, ordered by distance, returns only
	// visible rows.
	vec := pgvector.NewVector(embeddings.FakeVector("zebracorn", 768))
	n := w.count(attacker, `SELECT count(*) FROM (SELECT memory_id FROM memory_embeddings
		ORDER BY embedding::vector(768) <=> $1::vector(768) LIMIT 100) v
		WHERE memory_id NOT IN (SELECT id FROM memories)`, vec)
	if n != 0 {
		t.Errorf("vector scan returned %d rows outside the attacker's visible memories", n)
	}
	if visible := w.count(attacker, "SELECT count(DISTINCT scope_id) FROM memory_embeddings"); visible > 3 {
		t.Errorf("attacker sees embeddings of %d scopes; at most its chain of 3", visible)
	}
	// Other organizations do not exist for it.
	if n := w.count(attacker, "SELECT count(*) FROM scopes WHERE path LIKE 'globex%'"); n != 0 {
		t.Errorf("another organization's scopes are visible: %d", n)
	}

	writes := map[string]string{
		"rewrite revision":        "UPDATE memory_revisions SET content = 'x' WHERE memory_id = $1",
		"delete revision":         "DELETE FROM memory_revisions WHERE memory_id = $1",
		"rewrite event":           "UPDATE memory_events SET event_type = 'approved' WHERE memory_id = $1",
		"delete audit":            "DELETE FROM audit_events WHERE target_id = $1::text",
		"rewrite audit":           "UPDATE audit_events SET outcome = 'success' WHERE target_id = $1::text",
		"truncate audit":          "TRUNCATE audit_events",
		"self-grant":              "INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade, granted_by) SELECT org_id, id, tm_principal(), 'maintainer', true, tm_principal() FROM scopes WHERE id = (SELECT scope_id FROM memories WHERE id = $1)",
		"forge access":            "INSERT INTO scope_access (principal_id, scope_id, org_id, rank) SELECT tm_principal(), scope_id, org_id, 3 FROM memories WHERE id = $1",
		"refresh access":          "SELECT tm_refresh_scope_access(tm_principal()) WHERE $1::uuid IS NOT NULL",
		"memory in other scope":   "INSERT INTO memories (org_id, scope_id, kind, trust, current_revision, created_by) SELECT s.org_id, s.id, 'fact', 'unverified', 1, tm_principal() FROM scopes s WHERE s.path = 'acme/platform/payments' AND $1::uuid IS NOT NULL",
		"approve directly":        "UPDATE memories SET trust = 'approved', state_version = state_version + 1 WHERE id = $1",
		"delete memory":           "DELETE FROM memories WHERE id = $1",
		"insert embedding":        "INSERT INTO memory_embeddings (memory_id, revision, org_id, scope_id, model, dimensions, embedding) SELECT id, 1, org_id, scope_id, 'nomic-embed-text', 768, array_fill(0.1, ARRAY[768])::vector FROM memories WHERE id = $1",
		"set org owner":           "UPDATE org_settings SET owner_principal_id = tm_principal() WHERE $1::uuid IS NOT NULL",
		"drive promotion definer": "SELECT tm_promotion_finalize_source($1, $1)",
	}
	for name, sql := range writes {
		var n int64 = -1
		err := w.asApp(attacker, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql, pay.ID)
			n = tag.RowsAffected()
			return err
		})
		if err == nil && n > 0 {
			t.Errorf("%s: succeeded on %d rows", name, n)
		}
	}
	// Even in its own scope the app role cannot rewrite history or approve
	// without a browser review, and every memory change must go through the
	// lifecycle guard.
	own := w.mem["acme/platform/billing"]
	for name, sql := range map[string]string{
		"own revision rewrite": "UPDATE memory_revisions SET content = 'x' WHERE memory_id = $1",
		"own approve directly": "UPDATE memories SET trust = 'approved', state_version = state_version + 1 WHERE id = $1",
		"own move":             "UPDATE memories SET scope_id = (SELECT id FROM scopes WHERE path = 'acme'), state_version = state_version + 1 WHERE id = $1",
		"own approval record":  "INSERT INTO approvals (org_id, scope_id, memory_id, revision, event_id, decision, reviewer_id) SELECT m.org_id, m.scope_id, m.id, 1, e.id, 'approved', tm_principal() FROM memories m JOIN memory_events e ON e.memory_id = m.id WHERE m.id = $1 LIMIT 1",
	} {
		err := w.asApp(w.billingWriter, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, own.ID)
			return err
		})
		if err == nil {
			t.Errorf("%s: succeeded", name)
		}
	}
}

// TestIdentityVariableIsTheTrustBoundary documents the residual risk: the
// app role sets the request identity itself, so SQL injected into the app
// process could impersonate another principal. RLS does not defend against
// that; parameterized queries and the Go layer do (see threat model).
func TestIdentityVariableIsTheTrustBoundary(t *testing.T) {
	w := newWorld(t)
	pay := w.mem["acme/platform/payments"]
	if n := w.count(w.billingWriter, "SELECT count(*) FROM memories WHERE id = $1", pay.ID); n != 0 {
		t.Fatal("billing writer reads payments")
	}
	n := 0
	_ = w.asApp(w.billingWriter, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SELECT set_config('app.principal_id', $1, true)", w.paymentsReader.ID.String()); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), "SELECT count(*) FROM memories WHERE id = $1", pay.ID).Scan(&n)
	})
	if n != 1 {
		t.Fatal("expected impersonation by resetting app.principal_id to work; update the threat model if this changed")
	}
}

// TestWorkerRoleIsNarrow: the worker reads what it embeds and nothing else.
func TestWorkerRoleIsNarrow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, sql := range []string{
		"SELECT count(*) FROM audit_events",
		"SELECT count(*) FROM scope_members",
		"SELECT count(*) FROM approvals",
		"SELECT count(*) FROM personal_access_tokens",
		"SELECT count(*) FROM oauth_refresh_tokens",
		"SELECT count(source_uri) FROM memory_revisions",
		"SELECT count(created_by) FROM memories",
	} {
		var n int
		if err := w.db.Worker.QueryRow(ctx, sql).Scan(&n); err == nil {
			t.Errorf("worker may run %q", sql)
		}
	}
	tx, err := w.db.Worker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "UPDATE memories SET updated_at = now()"); err == nil {
		t.Error("worker updated memories")
	}
}

// TestSearchUnderRLSUsesPolicyAndScopeFilter: the app's own search agrees
// with the database's view, so neither layer is the only filter.
func TestSearchUnderRLSUsesPolicyAndScopeFilter(t *testing.T) {
	w := newWorld(t)
	res, err := w.srch.Search(context.Background(), w.billingWriter, search.Input{Scope: "acme/platform/billing", Query: "zebracorn", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Results {
		if !slices.Contains([]string{"acme", "acme/platform", "acme/platform/billing"}, r.OriginScope) {
			t.Fatalf("search returned %s", r.OriginScope)
		}
	}
	if _, err := w.srch.Search(context.Background(), w.billingWriter, search.Input{Scope: "acme/platform/payments", Query: "zebracorn"}); !is(err, memory.ErrNotFound) {
		t.Fatalf("search in an unreadable scope: %v", err)
	}
}

// TestFunctionsIgnoreTempTables: PostgreSQL searches pg_temp first for
// tables unless search_path names it, and every role may create temporary
// tables by default. A definer function or guard trigger whose search_path
// omits pg_temp would read the caller's temporary shadow of a table it
// trusts. Raw SQL under the app role's own identity must not forge grants,
// access rows or approvals that way.
func TestFunctionsIgnoreTempTables(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	globex := w.scopes["globex"].ID
	own := w.mem["acme/platform/billing"].ID
	accessInGlobex := "SELECT count(*) FROM public.scope_access WHERE principal_id = tm_principal() AND org_id = $1"
	for _, a := range []struct {
		name     string
		attacker memory.Principal
		arg      any
		stmts    []string
		check    string // must count 0
	}{
		{
			// tm_sync_idp_grants trusts operator-managed mappings.
			name: "shadow idp_group_mappings", attacker: w.paymentsMaint, arg: globex,
			stmts: []string{
				"CREATE TEMP TABLE idp_group_mappings (org_id uuid, issuer text, group_name text, scope_id uuid, role text, cascade boolean)",
				"INSERT INTO idp_group_mappings VALUES ($1, 'https://evil.example', 'g', $1, 'maintainer', true)",
				"SELECT tm_sync_idp_grants(tm_principal(), 'https://evil.example', ARRAY['g'])",
			},
			check: accessInGlobex,
		},
		{
			// tm_refresh_scope_access (run by the membership trigger)
			// trusts the closure table.
			name: "shadow scope_closure", attacker: w.paymentsMaint, arg: globex,
			stmts: []string{
				"CREATE TEMP TABLE scope_closure (LIKE public.scope_closure)",
				"INSERT INTO scope_closure SELECT $1, s.id, $1, 0 FROM public.scopes s WHERE s.path = 'acme/platform/payments'",
				"UPDATE scope_members SET updated_at = now(), granted_by = tm_principal() WHERE principal_id = tm_principal()",
			},
			check: accessInGlobex,
		},
		{
			// The memories guard trusts approvals when trust becomes
			// approved; a writer must not approve its own memory.
			name: "shadow approvals", attacker: w.billingWriter, arg: own,
			stmts: []string{
				"CREATE TEMP TABLE approvals (memory_id uuid, revision integer, decision text, decided_at timestamptz, id bigint)",
				"INSERT INTO approvals VALUES ($1, 1, 'approved', now(), 1)",
				"UPDATE public.memories SET trust = 'approved', state_version = state_version + 1 WHERE id = $1",
			},
			check: "SELECT count(*) FROM public.memories WHERE id = $1 AND trust = 'approved'",
		},
	} {
		forged := -1
		err := w.asApp(a.attacker, func(tx pgx.Tx) error {
			for _, sql := range a.stmts {
				var args []any
				if strings.Contains(sql, "$1") {
					args = append(args, a.arg)
				}
				if _, err := tx.Exec(ctx, sql, args...); err != nil {
					forged = 0 // refused
					return nil
				}
			}
			return tx.QueryRow(ctx, a.check, a.arg).Scan(&forged)
		})
		if err != nil {
			t.Fatalf("%s: %v", a.name, err)
		}
		if forged != 0 {
			t.Errorf("%s: forged %d rows", a.name, forged)
		}
	}
}
