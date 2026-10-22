// Package vectorbench measures filtered vector search over a synthetic
// dataset loaded through the real schema. It backs
// docs/decisions/vector-index-strategy.md and runs only when
// TEAM_MEMORY_VECTOR_BENCH is set, e.g.
//
//	TEAM_MEMORY_VECTOR_BENCH=20000,100000 make bench-vector
//
// Results are printed and written to TEAM_MEMORY_BENCH_OUT (default
// vectorbench-results.json in the package directory).
package vectorbench

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/testdb"
)

const (
	dims    = 768
	topics  = 256
	k       = 50 // the search service's candidate limit
	queries = 50
)

var model = embeddings.Model{Name: "nomic-embed-text", Dimensions: dims}

// strategy is one way of running the production vector query.
type strategy struct {
	Name     string
	Settings map[string]string
	// Exact rewrites ORDER BY so the HNSW index cannot be used; it is the
	// ground truth and the "filter first, then scan" option.
	Exact bool
}

var strategies = []strategy{
	{Name: "exact (btree filter + sort)", Exact: true},
	{Name: "planner default, iterative off, ef 40", Settings: map[string]string{"hnsw.iterative_scan": "off", "hnsw.ef_search": "40"}},
	{Name: "planner default, relaxed_order, ef 100", Settings: map[string]string{"hnsw.iterative_scan": "relaxed_order", "hnsw.ef_search": "100"}},
	{Name: "forced HNSW, iterative off, ef 40", Settings: forced("off", "40")},
	{Name: "forced HNSW, iterative off, ef 200", Settings: forced("off", "200")},
	{Name: "forced HNSW, relaxed_order, ef 100", Settings: forced("relaxed_order", "100")},
	{Name: "forced HNSW, strict_order, ef 100", Settings: forced("strict_order", "100")},
}

func forced(iter, ef string) map[string]string {
	return map[string]string{"hnsw.iterative_scan": iter, "hnsw.ef_search": ef,
		"enable_bitmapscan": "off", "enable_seqscan": "off", "enable_sort": "off"}
}

type scenario struct {
	Name     string
	ScopeIDs []uuid.UUID
	Rows     int // embeddings in the candidate scopes
}

type measurement struct {
	Size      int     `json:"size"`
	Scenario  string  `json:"scenario"`
	Fraction  float64 `json:"filter_fraction"`
	Strategy  string  `json:"strategy"`
	RecallAtK float64 `json:"recall_at_50"`
	Recall10  float64 `json:"recall_at_10"`
	MinRows   int     `json:"min_rows_returned"`
	P50ms     float64 `json:"p50_ms"`
	P95ms     float64 `json:"p95_ms"`
	Plan      string  `json:"plan"`
	// RLS is true when queries ran as the app role under row-level security
	// (TEAM_MEMORY_BENCH_RLS=1), as serve runs them since stage 04.
	RLS bool `json:"rls,omitempty"`
}

// benchPrincipal, when set, is the request identity queries run as on the
// app-role pool (RLS mode).
var benchPrincipal string

func TestVectorIndexStrategies(t *testing.T) {
	spec := os.Getenv("TEAM_MEMORY_VECTOR_BENCH")
	if spec == "" {
		t.Skip("set TEAM_MEMORY_VECTOR_BENCH=<sizes> to run the vector index benchmark")
	}
	var all []measurement
	for _, s := range strings.Split(spec, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, runSize(t, n)...)
	}
	out := os.Getenv("TEAM_MEMORY_BENCH_OUT")
	if out == "" {
		out = "vectorbench-results.json"
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

func runSize(t *testing.T, n int) []measurement {
	ctx := context.Background()
	d := testdb.NewDB(t)
	pool := d.Admin
	rng := rand.New(rand.NewPCG(uint64(n), 42))
	centroids := make([][]float32, topics)
	for i := range centroids {
		centroids[i] = randomUnit(rng, nil, 0)
	}

	start := time.Now()
	ds := load(t, ctx, pool, rng, centroids, n)
	t.Logf("n=%d: loaded in %s", n, time.Since(start).Round(time.Millisecond))
	start = time.Now()
	// The migration created the index on an empty table; rebuild it with the
	// data present, as an operator would after a bulk load, and refresh stats.
	// The build is serial: a parallel build allocates maintenance_work_mem in
	// dynamic shared memory, which exceeds a container's default 64 MB /dev/shm.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Exec(ctx, `SET maintenance_work_mem = '2GB'; SET max_parallel_maintenance_workers = 0;
		REINDEX INDEX `+model.IndexName())
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE memories; ANALYZE memory_embeddings; ANALYZE memory_revisions"); err != nil {
		t.Fatal(err)
	}
	var idxSize string
	_ = pool.QueryRow(ctx, "SELECT pg_size_pretty(pg_relation_size($1))", model.IndexName()).Scan(&idxSize)
	t.Logf("n=%d: HNSW build %s, index size %s", n, time.Since(start).Round(time.Millisecond), idxSize)

	// RLS mode: a principal with a cascading reader grant on every
	// organization sees every row, so recall compares like with like, and
	// queries pay the policy predicates on the app role.
	qpool, rls := pool, os.Getenv("TEAM_MEMORY_BENCH_RLS") == "1"
	benchPrincipal = ""
	if rls {
		var p uuid.UUID
		if err := pool.QueryRow(ctx, "INSERT INTO principals (kind, display_name) VALUES ('service', 'bench') RETURNING id").Scan(&p); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO scope_members (org_id, scope_id, principal_id, role, cascade)
			SELECT id, id, $1, 'reader', true FROM scopes WHERE parent_id IS NULL`, p); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, "ANALYZE scope_access"); err != nil {
			t.Fatal(err)
		}
		qpool, benchPrincipal = d.App, p.String()
	}

	qs := make([][]float32, queries)
	for i := range qs {
		qs[i] = randomUnit(rng, centroids[rng.IntN(topics)], 0.6)
	}
	var out []measurement
	for _, sc := range ds {
		frac := float64(sc.Rows) / float64(n)
		truth := make([][]uuid.UUID, len(qs))
		for i, q := range qs {
			truth[i], _ = runQuery(t, ctx, qpool, strategies[0], sc.ScopeIDs, q)
		}
		for _, st := range strategies {
			m := measurement{Size: n, Scenario: sc.Name, Fraction: frac, Strategy: st.Name, MinRows: k, RLS: rls}
			_, _ = runQuery(t, ctx, qpool, st, sc.ScopeIDs, qs[0]) // warm up
			var lat []float64
			var recall, recall10 float64
			for i, q := range qs {
				got, dur := runQuery(t, ctx, qpool, st, sc.ScopeIDs, q)
				lat = append(lat, float64(dur.Microseconds())/1000)
				recall += overlap(got, truth[i], k)
				recall10 += overlap(got, truth[i], 10)
				m.MinRows = min(m.MinRows, len(got))
			}
			m.RecallAtK, m.Recall10 = recall/float64(len(qs)), recall10/float64(len(qs))
			slices.Sort(lat)
			m.P50ms, m.P95ms = lat[len(lat)/2], lat[len(lat)*95/100]
			m.Plan = plan(t, ctx, qpool, st, sc.ScopeIDs, qs[0])
			t.Logf("n=%-7d %-28s %6.2f%% %-42s recall@50 %.3f recall@10 %.3f minrows %2d p50 %6.2fms p95 %6.2fms  %s",
				n, sc.Name, frac*100, st.Name, m.RecallAtK, m.Recall10, m.MinRows, m.P50ms, m.P95ms, m.Plan)
			out = append(out, m)
		}
	}
	return out
}

// overlap is |got[:at] ∩ truth[:at]| / |truth[:at]|.
func overlap(got, truth []uuid.UUID, at int) float64 {
	want := truth[:min(at, len(truth))]
	if len(want) == 0 {
		return 1
	}
	have := got[:min(at, len(got))]
	hit := 0
	for _, id := range want {
		if slices.Contains(have, id) {
			hit++
		}
	}
	return float64(hit) / float64(len(want))
}

func querySQL(st strategy) string {
	order := fmt.Sprintf("%s <=> %s", model.VectorExpr("e"), model.ParamExpr(1))
	if st.Exact {
		order = "(" + order + ") + 0"
	}
	// Same shape as internal/search.vectorCandidates.
	return fmt.Sprintf(`SELECT e.memory_id
FROM memory_embeddings e
JOIN memories mem ON mem.id = e.memory_id AND mem.current_revision = e.revision AND mem.scope_id = e.scope_id
WHERE %s AND e.scope_id = ANY($2) AND mem.status = 'active'
ORDER BY %s
LIMIT %d`, model.Predicate("e"), order, k)
}

func withSettings(ctx context.Context, pool *pgxpool.Pool, st strategy, fn func(pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// As in internal/search: plan each query for its own scope list. Without
	// this, pgx's cached statements let strategies share one generic plan.
	if _, err := tx.Exec(ctx, "SELECT set_config('plan_cache_mode', 'force_custom_plan', true), set_config('app.principal_id', $1, true)", benchPrincipal); err != nil {
		return err
	}
	for k, v := range st.Settings {
		if _, err := tx.Exec(ctx, "SELECT set_config($1, $2, true)", k, v); err != nil {
			return err
		}
	}
	return fn(tx)
}

func runQuery(t *testing.T, ctx context.Context, pool *pgxpool.Pool, st strategy, scopes []uuid.UUID, q []float32) ([]uuid.UUID, time.Duration) {
	t.Helper()
	var ids []uuid.UUID
	var d time.Duration
	err := withSettings(ctx, pool, st, func(tx pgx.Tx) error {
		start := time.Now()
		rows, err := tx.Query(ctx, querySQL(st), pgvector.NewVector(q), scopes)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		d = time.Since(start)
		return err
	})
	if err != nil {
		t.Fatalf("%s: %v", st.Name, err)
	}
	return ids, d
}

// plan summarizes the scan nodes PostgreSQL chose.
func plan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, st strategy, scopes []uuid.UUID, q []float32) string {
	t.Helper()
	var nodes []string
	err := withSettings(ctx, pool, st, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "EXPLAIN (FORMAT JSON) "+querySQL(st), pgvector.NewVector(q), scopes)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var doc []map[string]any
			if err := rows.Scan(&doc); err != nil {
				return err
			}
			walk(doc[0]["Plan"].(map[string]any), &nodes)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(nodes, " > ")
}

func walk(n map[string]any, out *[]string) {
	name := n["Node Type"].(string)
	if idx, ok := n["Index Name"].(string); ok {
		name += " on " + idx
	} else if rel, ok := n["Relation Name"].(string); ok && strings.Contains(name, "Scan") {
		name += " on " + rel
	}
	if strings.Contains(name, "Scan") || name == "Sort" || strings.HasPrefix(name, "Limit") || strings.Contains(name, "Join") || name == "Nested Loop" {
		*out = append(*out, name)
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			walk(k.(map[string]any), out)
		}
	}
}

func randomUnit(rng *rand.Rand, center []float32, noise float64) []float32 {
	v := make([]float32, dims)
	var norm float64
	for i := range v {
		x := rng.NormFloat64()
		if center != nil {
			x = float64(center[i])*math.Sqrt(dims) + noise*x
		}
		v[i] = float32(x)
		norm += x * x
	}
	norm = math.Sqrt(norm)
	for i := range v {
		v[i] = float32(float64(v[i]) / norm)
	}
	return v
}
