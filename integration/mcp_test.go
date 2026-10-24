package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/team-memory/team-memory/internal/auth"
	tmmcp "github.com/team-memory/team-memory/internal/mcp"
	"github.com/team-memory/team-memory/internal/memory"
)

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

type mcpEnv struct {
	url   string
	token string
	tr    *tree
}

func newMCPEnv(t *testing.T) *mcpEnv {
	t.Helper()
	tr := newTree(t)
	tok, err := auth.NewPATs(tr.db.Admin).Issue(context.Background(), tr.owner.ID, "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(tmmcp.Handler(tr.svc, tr.search, auth.NewPATs(tr.pool), tmmcp.Options{}))
	t.Cleanup(srv.Close)
	return &mcpEnv{url: srv.URL, token: tok.Secret, tr: tr}
}

func (e *mcpEnv) connect(t *testing.T, token, version string) (*mcp.ClientSession, error) {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   e.url,
		HTTPClient: &http.Client{Transport: bearer{token: token, base: http.DefaultTransport}},
		MaxRetries: -1,
	}
	var opts *mcp.ClientSessionOptions
	if version != "" {
		opts = &mcp.ClientSessionOptions{ProtocolVersion: version}
	}
	cs, err := client.Connect(context.Background(), transport, opts)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (map[string]any, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: non-JSON result %q", name, text)
	}
	return out, res.IsError
}

// TestMCPProtocol drives the server through the official Go SDK client with
// both the legacy initialize handshake (2025-11-25) and the stateless
// server/discover flow (2026-07-28).
func TestMCPProtocol(t *testing.T) {
	env := newMCPEnv(t)
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			cs, err := env.connect(t, env.token, version)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			if got := cs.InitializeResult().ProtocolVersion; got != version {
				t.Fatalf("negotiated %q, want %q", got, version)
			}
			if cs.InitializeResult().ServerInfo.Name != "team-memory" {
				t.Fatalf("server info: %+v", cs.InitializeResult().ServerInfo)
			}

			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tl := range tools.Tools {
				names = append(names, tl.Name)
			}
			want := "memory_approve,memory_create,memory_get,memory_history,memory_recent,memory_search,memory_supersede,memory_update"
			if got := strings.Join(sortStrings(names), ","); got != want {
				t.Fatalf("tools: %s", got)
			}

			scope := "acme/platform/payments"
			created, isErr := call(t, cs, "memory_create", map[string]any{"project": scope, "kind": "decision",
				"title": "Use PostgreSQL " + version, "content": "payments-api uses PostgreSQL " + version,
				"rationale": "ACID", "source_uri": "https://issues.example.com/PAY-1"})
			if isErr || created["trust"] != "cited" || created["revision"].(float64) != 1 {
				t.Fatalf("create: %v", created)
			}
			via := created["created_via"].(map[string]any)
			if via["channel"] != "mcp" || via["auth_method"] != "pat" {
				t.Fatalf("attribution: %v", via)
			}
			if version == "2026-07-28" && via["client_name"] != "integration-test" {
				t.Fatalf("client info: %v", via)
			}
			if _, ok := via["credential_id"]; ok {
				t.Fatal("credential ID leaked")
			}
			id := created["id"].(string)

			updated, isErr := call(t, cs, "memory_update", map[string]any{"id": id, "expected_revision": 1,
				"content": "payments-api uses PostgreSQL 17 " + version, "change_note": "pin version"})
			if isErr || updated["revision"].(float64) != 2 {
				t.Fatalf("update: %v", updated)
			}

			conflict, isErr := call(t, cs, "memory_update", map[string]any{"id": id, "expected_revision": 1, "title": "stale"})
			errBody, _ := conflict["error"].(map[string]any)
			if !isErr || errBody["code"] != "conflict" || errBody["current_revision"].(float64) != 2 {
				t.Fatalf("conflict: %v", conflict)
			}

			found, isErr := call(t, cs, "memory_search", map[string]any{"scope": scope, "query": "PostgreSQL 17",
				"types": []string{"decision"}, "limit": 5, "include_superseded": false, "include_overridden": false})
			results, _ := found["results"].([]any)
			hit := false
			for _, r := range results {
				m := r.(map[string]any)
				if m["id"] == id && m["revision"].(float64) == 2 && m["origin_scope"] == scope && m["inherited"] == false &&
					m["precedence"].(float64) == 2 && m["trust"] != nil && m["score"].(float64) > 0 &&
					m["updated_at"] != nil && len(m["retrievers"].([]any)) > 0 {
					hit = true
				}
			}
			meta, _ := found["meta"].(map[string]any)
			if isErr || !hit || meta["mode"] == nil || meta["score_note"] == nil {
				t.Fatalf("search: %v", found)
			}
			if other, _ := call(t, cs, "memory_search", map[string]any{"scope": scope, "query": "PostgreSQL 17",
				"types": []string{"lesson"}}); len(other["results"].([]any)) != 0 {
				t.Fatalf("types filter: %v", other)
			}

			hist, isErr := call(t, cs, "memory_history", map[string]any{"id": id})
			revs, _ := hist["revisions"].([]any)
			if isErr || len(revs) != 2 {
				t.Fatalf("history: %v", hist)
			}

			got, isErr := call(t, cs, "memory_get", map[string]any{"id": id})
			if isErr || got["created_by_name"] != "Owner" {
				t.Fatalf("get: %v", got)
			}

			// memory_approve requests review; it never approves.
			approve, isErr := call(t, cs, "memory_approve", map[string]any{"id": id, "revision": 2})
			if isErr || approve["review_requested"] != true || approve["approved"] != false || approve["trust"] != "cited" {
				t.Fatalf("approve: %v", approve)
			}
			again, isErr := call(t, cs, "memory_approve", map[string]any{"id": id, "revision": 2})
			if isErr || again["review_requested"] != false || again["already_requested"] != true {
				t.Fatalf("repeated approve is not idempotent: %v", again)
			}
			if got, _ := call(t, cs, "memory_get", map[string]any{"id": id}); got["trust"] != "cited" {
				t.Fatalf("memory_approve changed trust: %v", got)
			}

			recent, isErr := call(t, cs, "memory_recent", map[string]any{"scope": scope})
			if r, _ := recent["results"].([]any); isErr || len(r) == 0 {
				t.Fatalf("recent: %v", recent)
			}

			missing, isErr := call(t, cs, "memory_search", map[string]any{"query": "x"})
			if e, _ := missing["error"].(map[string]any); !isErr || e["code"] != "invalid_argument" {
				t.Fatalf("missing scope: %v", missing)
			}

			tmpl, err := cs.ListResourceTemplates(context.Background(), nil)
			if err != nil || len(tmpl.ResourceTemplates) != 8 {
				t.Fatalf("resource templates: %v %d", err, len(tmpl.ResourceTemplates))
			}
			rr, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "memory://scopes/" + scope + "/decisions"})
			if err != nil || !strings.Contains(rr.Contents[0].Text, id) {
				t.Fatalf("read resource: %v", err)
			}
			if _, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "memory://scopes/acme/nope/overview"}); err == nil {
				t.Fatal("unknown scope resource should fail")
			}
		})
	}
}

func sortStrings(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestMCPRequiresAuthentication(t *testing.T) {
	env := newMCPEnv(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`)
	for name, header := range map[string]string{
		"no header":    "",
		"bad token":    "Bearer " + auth.TokenPrefix + "invalid",
		"wrong scheme": "Basic " + env.token,
	} {
		req, _ := http.NewRequest(http.MethodPost, env.url, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	if _, err := env.connect(t, "tmpat_bogus", ""); err == nil {
		t.Fatal("SDK client connected with a bogus token")
	}

	// A revoked token stops working immediately.
	pats := auth.NewPATs(env.tr.db.Admin)
	tok, err := pats.Issue(context.Background(), env.tr.owner.ID, "revoke-me", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.connect(t, tok.Secret, ""); err != nil {
		t.Fatalf("fresh token: %v", err)
	}
	if err := pats.Revoke(context.Background(), tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.connect(t, tok.Secret, ""); err == nil {
		t.Fatal("revoked token still connects")
	}
}

// A principal only sees what their grants allow through MCP as well.
func TestMCPRespectsScopes(t *testing.T) {
	env := newMCPEnv(t)
	ctx := context.Background()
	env.tr.create(t, env.tr.owner, "acme/platform/billing", "fact", "billing secret plan", "billing only")
	reader := env.tr.member(t, "Payments reader", "acme/platform/payments", memory.RoleReader, false)
	tok, err := auth.NewPATs(env.tr.db.Admin).Issue(ctx, reader.ID, "reader", 0)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := env.connect(t, tok.Secret, "")
	if err != nil {
		t.Fatal(err)
	}
	res, isErr := call(t, cs, "memory_search", map[string]any{"scope": "acme/platform/billing", "query": "billing"})
	if e, _ := res["error"].(map[string]any); !isErr || e["code"] != "not_found" {
		t.Fatalf("sibling scope search: %v", res)
	}
	res, isErr = call(t, cs, "memory_create", map[string]any{"scope": "acme/platform/payments", "kind": "fact", "title": "t", "content": "c"})
	if e, _ := res["error"].(map[string]any); !isErr || e["code"] != "permission_denied" {
		t.Fatalf("reader create: %v", res)
	}
}

// TestMCPSearchContract exercises memory_search's inputs through the
// protocol: the project and kinds aliases, include_superseded and
// include_overridden.
func TestMCPSearchContract(t *testing.T) {
	env := newMCPEnv(t)
	ctx := context.Background()
	tr := env.tr
	org := tr.create(t, tr.owner, "acme", "convention", "Logging library", "use zap for structured logging")
	old := tr.create(t, tr.owner, "acme/platform/payments", "decision", "Queue choice", "payments uses RabbitMQ for refunds")
	if _, _, err := tr.svc.Supersede(ctx, tr.owner, memory.SupersedeInput{ID: old.ID, ExpectedRevision: 1,
		Replacement: memory.CreateInput{Title: "Queue choice", Content: "payments uses Kafka for refunds"}}); err != nil {
		t.Fatal(err)
	}
	pay, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "convention",
		Title: "Payments logging", Content: "payments uses slog", Overrides: []uuid.UUID{org.ID}})
	if err != nil {
		t.Fatal(err)
	}
	approve(t, tr, pay.ID)
	cs, err := env.connect(t, env.token, "")
	if err != nil {
		t.Fatal(err)
	}
	find := func(args map[string]any) (map[string]map[string]any, map[string]any) {
		t.Helper()
		res, isErr := call(t, cs, "memory_search", args)
		if isErr {
			t.Fatalf("memory_search %v: %v", args, res)
		}
		out := map[string]map[string]any{}
		for _, r := range res["results"].([]any) {
			m := r.(map[string]any)
			out[m["id"].(string)] = m
		}
		return out, res["meta"].(map[string]any)
	}

	got, _ := find(map[string]any{"project": "acme/platform/payments", "query": "refunds queue", "kinds": []string{"decision"}})
	if _, ok := got[old.ID.String()]; ok || len(got) != 1 {
		t.Fatalf("project/kinds aliases or superseded exclusion: %v", got)
	}
	got, _ = find(map[string]any{"project": "acme/platform/payments", "query": "refunds queue", "include_superseded": true})
	if r, ok := got[old.ID.String()]; !ok || r["status"] != "superseded" {
		t.Fatalf("include_superseded: %v", got)
	}

	got, meta := find(map[string]any{"scope": "acme/platform/payments", "query": "zap structured logging"})
	if _, ok := got[org.ID.String()]; ok || meta["overrides_in_effect"].(float64) != 1 {
		t.Fatalf("approved override not applied: %v %v", got, meta)
	}
	got, _ = find(map[string]any{"scope": "acme/platform/payments", "query": "zap structured logging", "include_overridden": true})
	r, ok := got[org.ID.String()]
	if by, _ := r["overridden_by"].([]any); !ok || len(by) != 1 || by[0] != pay.ID.String() {
		t.Fatalf("include_overridden: %v", got)
	}
}
