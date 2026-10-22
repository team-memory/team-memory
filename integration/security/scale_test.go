package security

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// TestDeepTreeManyMembers builds a 16-level chain with 20 sibling leaves
// per level (316 scopes), 2,000 principals with 4,000 grants and 6,000
// memories, then checks that visibility stays exact and that RLS-filtered
// reads and membership changes stay fast. Timings are logged for
// docs/decisions/rls.md; the thresholds are generous so the test catches
// pathological plans, not ordinary machine variance.
func TestDeepTreeManyMembers(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.admin.ConfigureOrg(ctx, "acme", []string{"organization", "team", "project", "area"}, 16); err != nil {
		t.Fatal(err)
	}
	chain := []string{"acme"}
	parent := "acme"
	for depth := 1; depth < 16; depth++ {
		sc, err := w.admin.CreateScope(ctx, uuid.Nil, parent, "area", fmt.Sprintf("d%d", depth), "")
		if err != nil {
			t.Fatal(err)
		}
		for leaf := 0; leaf < 20; leaf++ {
			if _, err := w.admin.CreateScope(ctx, uuid.Nil, parent, "area", fmt.Sprintf("d%d-leaf%d", depth, leaf), ""); err != nil {
				t.Fatal(err)
			}
		}
		parent = sc.Path
		chain = append(chain, sc.Path)
	}
	deepest := chain[len(chain)-1]

	// Bulk data through the operator connection.
	start := time.Now()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := w.db.Admin.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", strings.Fields(sql)[0], err)
		}
	}
	exec(`INSERT INTO principals (kind, display_name) SELECT 'human', 'bulk ' || g FROM generate_series(1, 2000) g`)
	exec(`INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade)
		SELECT s.org_id, s.id, p.id, (ARRAY['reader','writer','maintainer'])[1 + (abs(hashtext(p.id::text || s.id::text)) % 3)],
		       abs(hashtext(s.id::text || p.id::text)) % 4 = 0
		FROM (SELECT id, row_number() OVER (ORDER BY id) AS n FROM principals WHERE display_name LIKE 'bulk %') p
		CROSS JOIN LATERAL (SELECT id, org_id FROM scopes WHERE org_id = $1 AND path <> 'acme'
		                    ORDER BY md5(id::text || p.n::text) LIMIT 2) s`, w.scopes["acme"].ID)
	exec(`WITH m AS (
		INSERT INTO memories (org_id, scope_id, kind, trust, current_revision, created_by)
		SELECT s.org_id, s.id, 'fact', 'unverified', 1, $2 FROM scopes s, generate_series(1, 19) g WHERE s.org_id = $1
		RETURNING id, org_id, scope_id)
		INSERT INTO memory_revisions (memory_id, revision, org_id, scope_id, kind, title, content, changed_by)
		SELECT m.id, 1, m.org_id, m.scope_id, 'fact', 'bulk', 'bulk quokka content for ' || s.path, $2
		FROM m JOIN scopes s ON s.id = m.scope_id`, w.scopes["acme"].ID, w.owner.ID)
	t.Logf("bulk load: %s", time.Since(start).Round(time.Millisecond))
	var accessRows int
	_ = w.db.Admin.QueryRow(ctx, "SELECT count(*) FROM scope_access").Scan(&accessRows)
	t.Logf("scope_access rows: %d", accessRows)

	deep := w.member("Deep reader", deepest, memory.RoleReader, false)
	leaf := w.member("Leaf reader", "acme/d1/d2/d3/d4/d5/d6/d7/d8/d9/d10/d11/d12/d13/d14/d15-leaf3", memory.RoleReader, false)

	// Exact visibility: the deep reader sees its chain, the leaf reader its
	// own chain, never each other's.
	timed := func(p memory.Principal, scope string) ([]search.Result, time.Duration) {
		t.Helper()
		var best time.Duration
		var res search.Response
		for i := 0; i < 5; i++ {
			s := time.Now()
			var err error
			res, err = w.srch.Search(ctx, p, search.Input{Scope: scope, Query: "quokka", Limit: 50})
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(s); best == 0 || d < best {
				best = d
			}
		}
		return res.Results, best
	}
	res, d := timed(deep, deepest)
	t.Logf("search from depth 15 (16 visible scopes): %d results, best of 5 %s", len(res), d.Round(time.Microsecond))
	for _, r := range res {
		if !slices.Contains(chain, r.OriginScope) {
			t.Fatalf("deep reader saw %s", r.OriginScope)
		}
	}
	res, _ = timed(leaf, "acme/d1/d2/d3/d4/d5/d6/d7/d8/d9/d10/d11/d12/d13/d14/d15-leaf3")
	for _, r := range res {
		if r.OriginScope == deepest {
			t.Fatal("sibling leaf reader saw the deepest scope")
		}
	}
	if d > 2*time.Second {
		t.Fatalf("deep search took %s", d)
	}
	if _, err := w.svc.Get(ctx, leaf, w.mem["acme/platform/payments"].ID, ""); !is(err, memory.ErrNotFound) {
		t.Fatalf("leaf reader reached payments: %v", err)
	}

	// Raw RLS cost: count every visible memory as a bulk member.
	var bulk memory.Principal
	_ = w.db.Admin.QueryRow(ctx, "SELECT id FROM principals WHERE display_name = 'bulk 7'").Scan(&bulk.ID)
	s := time.Now()
	n := w.count(bulk, "SELECT count(*) FROM memories")
	t.Logf("RLS count(*) over %d memories as a bulk member: %d visible, %s", 6000, n, time.Since(s).Round(time.Microsecond))

	// Membership changes stay cheap: a cascading root grant covering 316
	// scopes, then its revocation.
	s = time.Now()
	big := w.member("Root cascade", "acme", memory.RoleReader, true)
	grant := time.Since(s)
	if got := w.count(big, "SELECT count(*) FROM scope_access WHERE principal_id = tm_principal()"); got < 316 {
		t.Fatalf("cascading root grant reached %d scopes", got)
	}
	s = time.Now()
	if err := w.admin.RemoveMember(ctx, "acme", big.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("cascading root grant: %s; revocation: %s", grant.Round(time.Microsecond), time.Since(s).Round(time.Microsecond))
	if got := w.count(big, "SELECT count(*) FROM memories"); got != 0 {
		t.Fatalf("revoked cascade still sees %d memories", got)
	}
}
