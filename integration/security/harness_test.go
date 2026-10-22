// Package security holds the adversarial test suite: each test tries to
// break tenant isolation, the scope hierarchy, the review workflow or the
// authentication boundary against real PostgreSQL, running code under test
// as the production database roles. Set TEST_DATABASE_URL (see
// internal/testdb).
package security

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/team-memory/team-memory/internal/auth"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/server"
	"github.com/team-memory/team-memory/internal/testdb"
)

// world is the adversarial fixture:
//
//	acme (org)                      globex (org, another tenant)
//	├── platform (team)             └── vault (project)
//	│   ├── payments (project)
//	│   └── billing (project)
//	└── data (team)
//	    └── warehouse (project)
//
// Every scope holds one memory; principals hold the narrowest grants that
// make each attack meaningful.
type world struct {
	t     *testing.T
	db    *testdb.DB
	svc   *memory.Service // app role
	admin *memory.Service // operator
	srch  *search.Service

	owner          memory.Principal // maintainer, cascading on acme (from init)
	orgOnly        memory.Principal // reader on acme, not cascading
	teamReader     memory.Principal // reader on platform, not cascading
	teamMaintainer memory.Principal // maintainer on platform, not cascading
	paymentsReader memory.Principal
	paymentsWriter memory.Principal
	paymentsMaint  memory.Principal // maintainer on payments only
	billingWriter  memory.Principal
	dataCascade    memory.Principal // writer on data, cascading
	globexOwner    memory.Principal // maintainer on globex, cascading
	agent          memory.Principal // service principal, maintainer on payments
	nobody         memory.Principal // no grants
	mem            map[string]memory.Memory
	scopes         map[string]memory.Scope
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	d := testdb.NewDB(t)
	w := &world{t: t, db: d, svc: memory.NewService(d.App), admin: memory.NewService(d.Admin),
		srch: search.New(d.App, search.Config{}), mem: map[string]memory.Memory{}, scopes: map[string]memory.Scope{}}
	w.owner = w.principal("Owner", "human")
	w.globexOwner = w.principal("Globex owner", "human")
	for _, org := range []struct {
		slug  string
		owner memory.Principal
	}{{"acme", w.owner}, {"globex", w.globexOwner}} {
		sc, err := w.admin.CreateOrganization(ctx, org.owner.ID, org.slug, org.slug)
		if err != nil {
			t.Fatal(err)
		}
		w.scopes[org.slug] = sc
		w.grant(org.slug, org.owner, memory.RoleMaintainer, true)
	}
	for _, s := range []struct{ parent, level, slug string }{
		{"acme", "team", "platform"}, {"acme/platform", "project", "payments"}, {"acme/platform", "project", "billing"},
		{"acme", "team", "data"}, {"acme/data", "project", "warehouse"}, {"globex", "project", "vault"},
	} {
		sc, err := w.admin.CreateScope(ctx, uuid.Nil, s.parent, s.level, s.slug, "")
		if err != nil {
			t.Fatal(err)
		}
		w.scopes[sc.Path] = sc
	}
	w.orgOnly = w.member("Org-only reader", "acme", memory.RoleReader, false)
	w.teamReader = w.member("Team reader", "acme/platform", memory.RoleReader, false)
	w.teamMaintainer = w.member("Team maintainer", "acme/platform", memory.RoleMaintainer, false)
	w.paymentsReader = w.member("Payments reader", "acme/platform/payments", memory.RoleReader, false)
	w.paymentsWriter = w.member("Payments writer", "acme/platform/payments", memory.RoleWriter, false)
	w.paymentsMaint = w.member("Payments maintainer", "acme/platform/payments", memory.RoleMaintainer, false)
	w.billingWriter = w.member("Billing writer", "acme/platform/billing", memory.RoleWriter, false)
	w.dataCascade = w.member("Data writer", "acme/data", memory.RoleWriter, true)
	w.agent = w.principal("CI agent", "service")
	w.grant("acme/platform/payments", w.agent, memory.RoleMaintainer, false)
	w.nobody = w.principal("Nobody", "human")

	for path, owner := range map[string]memory.Principal{
		"acme": w.owner, "acme/platform": w.owner, "acme/platform/payments": w.owner,
		"acme/platform/billing": w.owner, "acme/data": w.owner, "acme/data/warehouse": w.owner, "globex/vault": w.globexOwner,
	} {
		m, err := w.svc.Create(ctx, owner, memory.CreateInput{Scope: path, Kind: "decision",
			Title: "decision for " + path, Content: "shared keyword zebracorn; owned by " + path})
		if err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		w.mem[path] = m
	}
	return w
}

func (w *world) principal(name, kind string) memory.Principal {
	w.t.Helper()
	p, err := db.New(w.db.Admin).CreatePrincipal(context.Background(), db.CreatePrincipalParams{Kind: kind, DisplayName: name})
	if err != nil {
		w.t.Fatal(err)
	}
	return memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName, AuthMethod: memory.AuthPAT}
}

func (w *world) grant(scope string, p memory.Principal, role memory.Role, cascade bool) {
	w.t.Helper()
	if err := w.admin.AddMember(context.Background(), uuid.Nil, scope, p.ID, role, cascade); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) member(name, scope string, role memory.Role, cascade bool) memory.Principal {
	w.t.Helper()
	p := w.principal(name, "human")
	w.grant(scope, p, role, cascade)
	return p
}

func portal(p memory.Principal) memory.Principal {
	p.AuthMethod = memory.AuthPortal
	return p
}

func (w *world) token(p memory.Principal) string {
	w.t.Helper()
	tok, err := auth.NewPATs(w.db.Admin).Issue(context.Background(), p.ID, "test", 0)
	if err != nil {
		w.t.Fatal(err)
	}
	return tok.Secret
}

// serve runs the full HTTP server (as serve assembles it) on the app pool.
func (w *world) serve() string {
	w.t.Helper()
	srv := httptest.NewServer(server.New(server.Options{Pool: w.db.App, Search: w.srch, RatePerSecond: -1}))
	w.t.Cleanup(srv.Close)
	return srv.URL
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func connect(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "security-test", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &out); err != nil {
		t.Fatalf("%s: non-JSON result", name)
	}
	return out, res.IsError
}

func errCode(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

func is(err, target error) bool { return errors.Is(err, target) }

func errorsAs[T error](err error, target *T) bool { return errors.As(err, target) }

// asApp runs fn as raw SQL on the app role with p as the request identity
// and auth method set as a bearer-token call would set them, then rolls
// back: an attacker who can run arbitrary SQL under a fixed identity.
func (w *world) asApp(p memory.Principal, fn func(tx pgx.Tx) error) error {
	return w.asAppMethod(p, p.AuthMethod, fn)
}

func (w *world) asAppMethod(p memory.Principal, method string, fn func(tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := w.db.App.Begin(ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	principal := ""
	if p.ID != uuid.Nil {
		principal = p.ID.String()
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.principal_id', $1, true), set_config('app.auth_method', $2, true)", principal, method); err != nil {
		w.t.Fatal(err)
	}
	return fn(tx)
}

// count runs a count query as the app role with p's identity.
func (w *world) count(p memory.Principal, sql string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.asApp(p, func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), sql, args...).Scan(&n) }); err != nil {
		w.t.Fatalf("%s: %v", sql, err)
	}
	return n
}
