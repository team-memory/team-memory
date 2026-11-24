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

// probe attempts every read path of all nine tools and all four resources
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
	out, isErr = call(t, cs, "memory_scope_create", map[string]any{"parent": scope, "level": "project", "slug": "probe"})
	check("memory_scope_create", out, isErr)
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

// TestScopeCreationThroughMCP: memory_scope_create adds a scope only under
// a scope the caller holds a cascading maintainer grant on. No other
// position creates one, a parent the caller cannot see answers as a missing
// one, and repeated or concurrent calls end with one scope.
func TestScopeCreationThroughMCP(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	url := w.serve()
	scopes := func() int {
		t.Helper()
		var n int
		if err := w.db.Admin.QueryRow(ctx, "SELECT count(*) FROM scopes").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	bot := w.principal("Bot", "service")
	w.grant("acme/data", bot, memory.RoleMaintainer, true)
	before := scopes()
	refusals := []struct {
		who    memory.Principal
		parent string
		code   string
		why    string
	}{
		{w.nobody, "acme", "not_found", "principal with no grants"},
		{w.globexOwner, "acme/platform", "not_found", "another organization's owner"},
		{w.globexOwner, w.scopes["acme/platform"].ID.String(), "not_found", "another organization's owner, by id"},
		{w.dataCascade, "acme/platform", "not_found", "cascading writer under a sibling subtree"},
		{bot, "acme/platform", "not_found", "cascading maintainer under a sibling subtree"},
		{bot, "acme", "permission_denied", "cascading maintainer under its own parent"},
		{w.dataCascade, "acme/data", "permission_denied", "cascading writer"},
		{w.teamMaintainer, "acme/platform", "permission_denied", "maintainer without a cascading grant"},
		{w.agent, "acme/platform/payments", "permission_denied", "service maintainer without a cascading grant"},
		{w.paymentsMaint, "acme/platform", "permission_denied", "maintainer of a child, on the inherited parent"},
		{w.owner, "acme/nowhere", "not_found", "missing parent"},
		{w.owner, "", "invalid_argument", "no parent"},
	}
	for _, c := range refusals {
		cs := connect(t, url, w.token(c.who))
		out, isErr := call(t, cs, "memory_scope_create", map[string]any{"parent": c.parent, "level": "project", "slug": "sneaky"})
		if !isErr || errCode(out) != c.code {
			t.Errorf("%s: got %v, want %s", c.why, out, c.code)
		}
	}
	owner := connect(t, url, w.token(w.owner))
	for why, args := range map[string]map[string]any{
		"level the organization does not allow": {"parent": "acme/platform", "level": "galaxy", "slug": "sneaky"},
		"malformed level":                       {"parent": "acme/platform", "level": "Project", "slug": "sneaky"},
		"slug with a path separator":            {"parent": "acme/platform", "level": "project", "slug": "a/b"},
		"slug in upper case":                    {"parent": "acme/platform", "level": "project", "slug": "Sneaky"},
		"name too long":                         {"parent": "acme/platform", "level": "project", "slug": "sneaky", "name": strings.Repeat("n", 201)},
	} {
		if out, isErr := call(t, owner, "memory_scope_create", args); !isErr || errCode(out) != "invalid_argument" {
			t.Errorf("%s: %v", why, out)
		}
	}
	if n := scopes(); n != before {
		t.Fatalf("refused calls created %d scopes", n-before)
	}

	// A service principal with a cascading maintainer grant creates under
	// its subtree and can use the scope at once; outsiders cannot see it.
	cs := connect(t, url, w.token(bot))
	out, isErr := call(t, cs, "memory_scope_create", map[string]any{"parent": "acme/data", "level": "project", "slug": "lake", "name": "Lake"})
	sc, _ := out["scope"].(map[string]any)
	if isErr || out["created"] != true || sc["path"] != "acme/data/lake" || sc["level"] != "project" || sc["name"] != "Lake" {
		t.Fatalf("create: %v", out)
	}
	if out, isErr := call(t, cs, "memory_create", map[string]any{"scope": "acme/data/lake", "kind": "fact", "title": "t", "content": "c"}); isErr {
		t.Fatalf("creator cannot write to the scope it created: %v", out)
	}
	outsider := connect(t, url, w.token(w.billingWriter))
	if out, isErr := call(t, outsider, "memory_search", map[string]any{"scope": "acme/data/lake", "query": "t"}); !isErr || errCode(out) != "not_found" {
		t.Fatalf("outsider reached the new scope: %v", out)
	}

	// Repeating the call returns the same scope and creates nothing.
	again, isErr := call(t, cs, "memory_scope_create", map[string]any{"parent": "acme/data", "level": "project", "slug": "lake", "name": "Renamed"})
	sc2, _ := again["scope"].(map[string]any)
	if isErr || again["created"] != false || sc2["id"] != sc["id"] || sc2["name"] != "Lake" {
		t.Fatalf("repeat: %v", again)
	}
	if out, isErr := call(t, cs, "memory_scope_create", map[string]any{"parent": "acme/data", "level": "team", "slug": "lake"}); !isErr || errCode(out) != "invalid_argument" {
		t.Fatalf("existing scope at another level: %v", out)
	}
	// An existing scope is not returned to a caller who may not create it.
	if out, isErr := call(t, connect(t, url, w.token(w.dataCascade)), "memory_scope_create", map[string]any{"parent": "acme/data", "level": "project", "slug": "lake"}); !isErr || errCode(out) != "permission_denied" {
		t.Fatalf("cascading writer got the existing scope: %v", out)
	}
	var audited int
	if err := w.db.Admin.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action = 'scope.create' AND outcome = 'success' AND target_id = $1", sc["id"]).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("scope.create audit rows for one scope: %d %v", audited, err)
	}

	// Concurrent calls for one new scope all succeed and exactly one creates it.
	const racers = 8
	var wg sync.WaitGroup
	results := make([]string, racers)
	for i := range racers {
		rc := connect(t, url, w.token(bot))
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := rc.CallTool(ctx, &mcp.CallToolParams{Name: "memory_scope_create",
				Arguments: map[string]any{"parent": "acme/data", "level": "project", "slug": "raced"}})
			switch {
			case err != nil:
				results[i] = err.Error()
			case res.IsError:
				results[i] = res.Content[0].(*mcp.TextContent).Text
			case strings.Contains(res.Content[0].(*mcp.TextContent).Text, `"created": true`):
				results[i] = "created"
			default:
				results[i] = "existing"
			}
		}()
	}
	wg.Wait()
	created := 0
	for _, r := range results {
		switch r {
		case "created":
			created++
		case "existing":
		default:
			t.Errorf("concurrent create failed: %s", r)
		}
	}
	var rows int
	if err := w.db.Admin.QueryRow(ctx, "SELECT count(*) FROM scopes WHERE path = 'acme/data/raced'").Scan(&rows); err != nil || rows != 1 || created != 1 {
		t.Fatalf("concurrent creates: %d rows, %d reported created (%v) %v", rows, created, results, err)
	}

	// The tree's depth limit applies (the default maximum depth is 5).
	parent, code := "acme/data/lake", ""
	for range 5 {
		out, isErr := call(t, cs, "memory_scope_create", map[string]any{"parent": parent, "level": "project", "slug": "deeper"})
		if isErr {
			code = errCode(out)
			break
		}
		parent += "/deeper"
	}
	if code != "invalid_argument" {
		t.Fatalf("depth limit: stopped at %s with %q", parent, code)
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
