package security

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/server"
	"github.com/team-memory/team-memory/internal/telemetry"
	"github.com/team-memory/team-memory/internal/testdb"
)

// TestTelemetrySampleWorkload runs one MCP write and one search through the
// assembled server and checks that metrics, spans, logs' request id and the
// audit event are tied together: with no incoming X-Request-Id, the request
// id is the trace id.
func TestTelemetrySampleWorkload(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	guard := &postgres.TxGuard{}
	pool := w.db.Connect(t, testdb.AppUser, telemetry.MultiTracer{guard, telemetry.PGXTracer{}})
	m := telemetry.New("serve", "test")
	m.RegisterPool(pool, "app")
	srv := httptest.NewServer(server.New(server.Options{Pool: pool, Search: search.New(pool, search.Config{}), RatePerSecond: -1, Metrics: m}))
	t.Cleanup(srv.Close)

	cs := connect(t, srv.URL, w.token(w.paymentsWriter))
	out, isErr := call(t, cs, "memory_create", map[string]any{"scope": "acme/platform/payments", "kind": "fact",
		"title": "Telemetry fact", "content": "Observed."})
	if isErr {
		t.Fatalf("create: %v", out)
	}
	if _, isErr := call(t, cs, "memory_search", map[string]any{"scope": "acme/platform/payments", "query": "telemetry"}); isErr {
		t.Fatal("search failed")
	}
	if _, isErr := call(t, cs, "memory_get", map[string]any{"id": "00000000-0000-0000-0000-000000000001"}); !isErr {
		t.Fatal("get of a missing memory succeeded")
	}
	resp, err := http.Get(srv.URL + "/readyz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("readyz: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	if guard.Violations() != 0 {
		t.Fatalf("%d statements outside a transaction", guard.Violations())
	}

	// Metrics.
	mr := httptest.NewRecorder()
	m.Handler().ServeHTTP(mr, httptest.NewRequest("GET", "/metrics", nil))
	body, _ := io.ReadAll(mr.Body)
	for _, want := range []string{
		`team_memory_mcp_tool_calls_total{outcome="ok",tool="memory_create"} 1`,
		`team_memory_mcp_tool_calls_total{outcome="ok",tool="memory_search"} 1`,
		`team_memory_mcp_tool_calls_total{outcome="tool_error",tool="memory_get"} 1`,
		`team_memory_mcp_tool_duration_seconds_count{tool="memory_search"} 1`,
		`team_memory_http_requests_total{code="2xx",method="POST",route="/mcp"}`,
		`team_memory_http_request_duration_seconds_bucket{route="/readyz",le="+Inf"} 1`,
		`team_memory_db_pool_max_connections{pool="app"}`,
		`team_memory_ready 1`,
		`team_memory_build_info{component="serve",version="test"} 1`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Spans: the create call's HTTP span, tool span and database spans share
	// one trace, and the audit event carries that trace id as request id.
	var tool sdktrace.ReadOnlySpan
	for _, s := range rec.Ended() {
		if s.Name() == "mcp.tool memory_create" {
			tool = s
		}
	}
	if tool == nil {
		t.Fatal("no span for the memory_create tool call")
	}
	traceID := tool.SpanContext().TraceID().String()
	var names []string
	for _, s := range rec.Ended() {
		if s.SpanContext().TraceID().String() == traceID {
			names = append(names, s.Name())
		}
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "POST /mcp") || !strings.Contains(joined, "db InsertMemory") || !strings.Contains(joined, "db InsertAuditEvent") {
		t.Fatalf("spans in the create trace: %v", names)
	}
	var auditRequestID string
	if err := w.db.Admin.QueryRow(ctx, `SELECT request_id FROM audit_events WHERE action = 'memory.create' ORDER BY id DESC LIMIT 1`).Scan(&auditRequestID); err != nil {
		t.Fatal(err)
	}
	if auditRequestID != traceID {
		t.Fatalf("audit request_id %q, trace %q", auditRequestID, traceID)
	}
}
