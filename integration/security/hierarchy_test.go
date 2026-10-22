package security

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/search"
)

// probe attempts every read path of all eight tools and all four resources
// against a target scope and memory, and reports which ones leaked.
func probe(t *testing.T, cs *mcp.ClientSession, scope string, target memory.Memory) []string {
	t.Helper()
	var leaks []string
	id := target.ID.String()
	needle := target.Title
	check := func(what string, out map[string]any, isErr bool) {
		b := fmt.Sprint(out)
		if !isErr || strings.Contains(b, needle) || strings.Contains(b, "owned by "+target.OriginScope) {
			leaks = append(leaks, what)
		}
	}
	for _, args := range []map[string]any{{"scope": scope, "query": "zebracorn"}, {"project": scope, "query": "decision"}} {
		out, isErr := call(t, cs, "memory_search", args)
		check("memory_search", out, isErr)
	}
	out, isErr := call(t, cs, "memory_get", map[string]any{"id": id})
	check("memory_get", out, isErr)
	out, isErr = call(t, cs, "memory_get", map[string]any{"id": id, "scope": scope})
	check("memory_get+scope", out, isErr)
	out, isErr = call(t, cs, "memory_history", map[string]any{"id": id})
	check("memory_history", out, isErr)
	out, isErr = call(t, cs, "memory_recent", map[string]any{"scope": scope})
	check("memory_recent", out, isErr)
	out, isErr = call(t, cs, "memory_create", map[string]any{"scope": scope, "kind": "fact", "title": "probe", "content": "probe"})
	check("memory_create", out, isErr)
	out, isErr = call(t, cs, "memory_update", map[string]any{"id": id, "expected_revision": 1, "title": "hijacked"})
	check("memory_update", out, isErr)
	out, isErr = call(t, cs, "memory_supersede", map[string]any{"id": id, "expected_revision": 1, "title": "x", "content": "y"})
	check("memory_supersede", out, isErr)
	out, isErr = call(t, cs, "memory_approve", map[string]any{"id": id, "revision": 1})
	check("memory_approve", out, isErr)
	for _, view := range []string{"overview", "decisions", "conventions", "recent"} {
		uri := "memory://scopes/" + scope + "/" + view
		res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: uri})
		if err == nil {
			leaks = append(leaks, "resource "+view+": "+res.Contents[0].Text[:min(80, len(res.Contents[0].Text))])
		}
	}
	return leaks
}

// TestHierarchyIsolationThroughMCP: principals outside a scope's readers
// cannot reach its memories through any tool or resource, by search, by
// guessed ID, history, or writes, and the answer never reveals existence.
func TestHierarchyIsolationThroughMCP(t *testing.T) {
	w := newWorld(t)
	url := w.serve()
	cases := []struct {
		who    memory.Principal
		target string
		why    string
	}{
		{w.orgOnly, "acme/platform", "org-only member reaching a team"},
		{w.orgOnly, "acme/platform/payments", "org-only member reaching a project"},
		{w.teamReader, "acme/platform/payments", "non-cascading team grant reaching a child project"},
		{w.teamMaintainer, "acme/platform/payments", "non-cascading team maintainer reaching a child project"},
		{w.paymentsReader, "acme/platform/billing", "sibling project"},
		{w.dataCascade, "acme/platform/payments", "another team's cascading member"},
		{w.billingWriter, "acme/data/warehouse", "sibling team's project"},
		{w.owner, "globex/vault", "another organization (owner of acme)"},
		{w.globexOwner, "acme/platform/payments", "another organization"},
		{w.nobody, "acme", "principal with no grants"},
	}
	// Control: the probe does detect access where it exists, through every
	// read path (writes succeed too, which also count as a leak here).
	t.Run("control", func(t *testing.T) {
		cs := connect(t, url, w.token(w.paymentsMaint))
		leaks := probe(t, cs, "acme/platform/payments", w.mem["acme/platform/payments"])
		for _, want := range []string{"memory_search", "memory_get", "memory_get+scope", "memory_history", "memory_recent", "memory_create"} {
			if !strings.Contains(fmt.Sprint(leaks), want) {
				t.Errorf("probe did not see authorized access through %s: %v", want, leaks)
			}
		}
		if n := len(leaks); n < 10 {
			t.Errorf("probe saw only %d access paths for an authorized maintainer: %v", n, leaks)
		}
	})
	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			cs := connect(t, url, w.token(c.who))
			if leaks := probe(t, cs, c.target, w.mem[c.target]); len(leaks) > 0 {
				t.Fatalf("%s leaked through: %v", c.who.DisplayName, leaks)
			}
		})
	}
}

// TestInheritedMemoriesKeepOrigin: readers of a project see ancestor
// memories, labelled with their origin, and never descendant memories.
func TestInheritedMemoriesKeepOrigin(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	res, err := w.srch.Search(ctx, w.paymentsReader, search.Input{Scope: "acme/platform/payments", Query: "zebracorn"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range res.Results {
		got[r.OriginScope] = r.Inherited
	}
	want := map[string]bool{"acme": true, "acme/platform": true, "acme/platform/payments": false}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("visible origins %v, want %v", got, want)
	}
	// From the organization, nothing below is visible, even to the owner.
	res, err = w.srch.Search(ctx, w.owner, search.Input{Scope: "acme", Query: "zebracorn"})
	if err != nil || len(res.Results) != 1 || res.Results[0].OriginScope != "acme" {
		t.Fatalf("org-level search showed descendants: %v %+v", err, res.Results)
	}
	// An inherited memory cannot be viewed "from" a scope the reader cannot see.
	if _, err := w.svc.Get(ctx, w.paymentsReader, w.mem["acme"].ID, "acme/platform/billing"); !is(err, memory.ErrNotFound) {
		t.Fatalf("view from an unreadable scope: %v", err)
	}
	m, err := w.svc.Get(ctx, w.paymentsReader, w.mem["acme"].ID, "acme/platform/payments")
	if err != nil || !m.Inherited || m.OriginScope != "acme" {
		t.Fatalf("inherited view: %v %+v", err, m)
	}
}

// TestNoWritesAboveOwnScope: project members cannot write, supersede,
// archive, request review of or approve team- or org-level memories.
func TestNoWritesAboveOwnScope(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, p := range []memory.Principal{w.paymentsWriter, w.paymentsMaint} {
		for _, path := range []string{"acme", "acme/platform"} {
			m := w.mem[path]
			if _, err := w.svc.Update(ctx, p, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Title: strp("x")}); !is(err, memory.ErrInherited) {
				t.Errorf("%s update %s: %v", p.DisplayName, path, err)
			}
			if _, _, err := w.svc.Supersede(ctx, p, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 1,
				Replacement: memory.CreateInput{Title: "x", Content: "y"}}); !is(err, memory.ErrInherited) {
				t.Errorf("%s supersede %s: %v", p.DisplayName, path, err)
			}
			if _, err := w.svc.Archive(ctx, p, memory.ArchiveInput{ID: m.ID, ExpectedStateVersion: 1}); !is(err, memory.ErrInherited) {
				t.Errorf("%s archive %s: %v", p.DisplayName, path, err)
			}
			if _, err := w.svc.RequestReview(ctx, p, m.ID, 1); !is(err, memory.ErrInherited) {
				t.Errorf("%s review request %s: %v", p.DisplayName, path, err)
			}
			if _, err := w.svc.Approve(ctx, portal(p), memory.ReviewInput{ID: m.ID, Revision: 1}); !is(err, memory.ErrInherited) {
				t.Errorf("%s approve %s: %v", p.DisplayName, path, err)
			}
			if _, err := w.svc.Create(ctx, p, memory.CreateInput{Scope: path, Kind: "fact", Title: "t", Content: "c"}); !is(err, memory.ErrForbidden) {
				t.Errorf("%s create in %s: %v", p.DisplayName, path, err)
			}
		}
	}
}

// TestRolesPerAction: reader cannot write; writer cannot supersede,
// archive, request review or approve.
func TestRolesPerAction(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	m := w.mem["acme/platform/payments"]
	if _, err := w.svc.Create(ctx, w.paymentsReader, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "t", Content: "c"}); !is(err, memory.ErrForbidden) {
		t.Errorf("reader create: %v", err)
	}
	if _, err := w.svc.Update(ctx, w.paymentsReader, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Title: strp("x")}); !is(err, memory.ErrForbidden) {
		t.Errorf("reader update: %v", err)
	}
	if _, _, err := w.svc.Supersede(ctx, w.paymentsWriter, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Title: "x", Content: "y"}}); !is(err, memory.ErrForbidden) {
		t.Errorf("writer supersede: %v", err)
	}
	if _, err := w.svc.Archive(ctx, w.paymentsWriter, memory.ArchiveInput{ID: m.ID, ExpectedStateVersion: 1}); !is(err, memory.ErrForbidden) {
		t.Errorf("writer archive: %v", err)
	}
	if _, err := w.svc.RequestReview(ctx, w.paymentsWriter, m.ID, 1); !is(err, memory.ErrForbidden) {
		t.Errorf("writer review request: %v", err)
	}
	if _, err := w.svc.Approve(ctx, portal(w.paymentsWriter), memory.ReviewInput{ID: m.ID, Revision: 1}); !is(err, memory.ErrForbidden) {
		t.Errorf("writer approve: %v", err)
	}
	if err := w.svc.GrantMember(ctx, w.paymentsWriter, "acme/platform/payments", w.nobody.ID, memory.RoleReader, false); !is(err, memory.ErrForbidden) {
		t.Errorf("writer grant: %v", err)
	}
	// The writer's own memory: writable, but not supersedable.
	own, err := w.svc.Create(ctx, w.paymentsWriter, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact", Title: "mine", Content: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Update(ctx, w.paymentsWriter, memory.UpdateInput{ID: own.ID, ExpectedRevision: 1, Content: strp("c2")}); err != nil {
		t.Errorf("writer update own: %v", err)
	}
}

// TestNonCascadingGrantsAndAdministration: a non-cascading grant never
// reaches descendants, and a maintainer without a cascading grant cannot
// create child scopes or hand out cascading roles (which would open a
// subtree it cannot see).
func TestNonCascadingGrantsAndAdministration(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := w.svc.VisibleScopes(ctx, w.teamMaintainer, "acme/platform/payments"); !is(err, memory.ErrNotFound) {
		t.Fatalf("non-cascading team grant reached a project: %v", err)
	}
	if _, err := w.svc.CreateScopeAs(ctx, w.teamMaintainer, "acme/platform", "project", "sneaky", ""); !is(err, memory.ErrForbidden) {
		t.Fatalf("non-cascading maintainer created a child scope: %v", err)
	}
	if err := w.svc.GrantMember(ctx, w.teamMaintainer, "acme/platform", w.teamMaintainer.ID, memory.RoleMaintainer, true); !is(err, memory.ErrForbidden) {
		t.Fatalf("maintainer escalated itself to a cascading grant: %v", err)
	}
	if err := w.svc.GrantMember(ctx, w.teamMaintainer, "acme/platform", w.nobody.ID, memory.RoleReader, true); !is(err, memory.ErrForbidden) {
		t.Fatalf("maintainer granted a cascading role: %v", err)
	}
	if err := w.svc.GrantMember(ctx, w.teamMaintainer, "acme/platform/payments", w.nobody.ID, memory.RoleReader, false); !is(err, memory.ErrNotFound) {
		t.Fatalf("maintainer granted on an invisible child: %v", err)
	}
	// Allowed: a non-cascading grant on its own scope.
	if err := w.svc.GrantMember(ctx, w.teamMaintainer, "acme/platform", w.nobody.ID, memory.RoleReader, false); err != nil {
		t.Fatal(err)
	}
	// The organization owner can create scopes and keeps access to them.
	sc, err := w.svc.CreateScopeAs(ctx, w.owner, "acme/platform", "project", "ledger", "Ledger")
	if err != nil || sc.Path != "acme/platform/ledger" {
		t.Fatalf("owner create scope: %v %+v", err, sc)
	}
	if _, err := w.svc.Create(ctx, w.owner, memory.CreateInput{Scope: "acme/platform/ledger", Kind: "fact", Title: "t", Content: "c"}); err != nil {
		t.Fatalf("owner cannot use the scope it created: %v", err)
	}
	// The owner's root grant cannot be removed or downgraded through the API.
	if err := w.svc.RevokeMember(ctx, w.owner, "acme", w.owner.ID); !is(err, memory.ErrForbidden) {
		t.Fatalf("owner root grant revoked through the API: %v", err)
	}
}

// TestRevocationIsImmediate: a revoked principal loses access on its next
// request, across MCP connections and pooled database connections.
func TestRevocationIsImmediate(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	url := w.serve()
	alice := w.principal("Alice", "human")
	tok := w.token(alice)
	if err := w.svc.GrantMember(ctx, w.paymentsMaint, "acme/platform/payments", alice.ID, memory.RoleWriter, false); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, url, tok)
	id := w.mem["acme/platform/payments"].ID.String()
	// Warm every pooled connection with Alice's reads.
	for i := 0; i < 10; i++ {
		if out, isErr := call(t, cs, "memory_get", map[string]any{"id": id}); isErr {
			t.Fatalf("granted read failed: %v", out)
		}
	}
	if err := w.svc.RevokeMember(ctx, w.paymentsMaint, "acme/platform/payments", alice.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if out, isErr := call(t, cs, "memory_get", map[string]any{"id": id}); !isErr || errCode(out) != "not_found" {
			t.Fatalf("read %d after revocation: %v", i, out)
		}
		if out, isErr := call(t, cs, "memory_create", map[string]any{"scope": "acme/platform/payments", "kind": "fact", "title": "t", "content": "c"}); !isErr {
			t.Fatalf("write after revocation: %v", out)
		}
	}
	// Disabling the principal stops its token too.
	w.grant("acme/platform/payments", alice, memory.RoleReader, false)
	if _, err := w.db.Admin.Exec(ctx, "UPDATE principals SET disabled_at = now() WHERE id = $1", alice.ID); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "x", Version: "0"}, nil)
	if _, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: tok, base: http.DefaultTransport}}, MaxRetries: -1}, nil); err == nil {
		t.Fatal("disabled principal's token still connects")
	}
}

// TestScopesCannotMove: no API moves or re-parents a scope, and the app
// role cannot do it with SQL either.
func TestScopesCannotMove(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	err := w.asApp(w.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE scopes SET parent_id = $1 WHERE id = $2", w.scopes["acme/data"].ID, w.scopes["acme/platform/payments"].ID)
		return err
	})
	if err == nil {
		t.Fatal("app role re-parented a scope")
	}
	if _, err := w.db.Admin.Exec(ctx, "UPDATE scopes SET parent_id = $1 WHERE id = $2", w.scopes["acme/data"].ID, w.scopes["acme/platform/payments"].ID); err == nil {
		t.Fatal("even the owner may not move a scope")
	}
}

// TestPoolReuseDoesNotLeakIdentity runs interleaved concurrent requests for
// principals with disjoint visibility over a deliberately small pool, so
// connections are reused between them constantly.
func TestPoolReuseDoesNotLeakIdentity(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	type actor struct {
		p    memory.Principal
		own  string
		deny string
	}
	actors := []actor{
		{w.paymentsReader, "acme/platform/payments", "acme/platform/billing"},
		{w.billingWriter, "acme/platform/billing", "acme/platform/payments"},
		{w.globexOwner, "globex/vault", "acme/platform/payments"},
		{w.dataCascade, "acme/data/warehouse", "acme/platform/billing"},
	}
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for round := 0; round < 25; round++ {
		for _, a := range actors {
			wg.Add(1)
			go func(a actor) {
				defer wg.Done()
				if _, err := w.svc.Get(ctx, a.p, w.mem[a.own].ID, ""); err != nil {
					errs <- fmt.Errorf("%s lost access to own scope: %w", a.p.DisplayName, err)
				}
				if m, err := w.svc.Get(ctx, a.p, w.mem[a.deny].ID, ""); err == nil {
					errs <- fmt.Errorf("%s read %s through a reused connection", a.p.DisplayName, m.OriginScope)
				}
				res, err := w.srch.Search(ctx, a.p, search.Input{Scope: a.own, Query: "zebracorn", Limit: 50})
				if err != nil {
					errs <- err
					return
				}
				for _, r := range res.Results {
					if r.OriginScope == a.deny {
						errs <- fmt.Errorf("%s searched into %s", a.p.DisplayName, a.deny)
					}
				}
			}(a)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func strp(s string) *string { return &s }
