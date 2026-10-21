package vectorbench

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// load builds the synthetic tree and n memories with embeddings, and returns
// the search scenarios. Shape:
//
//	acme (organization)             2% of memories
//	  t00..t19 (team)               0.4% each
//	    p0..p9 (project)            0.4% each (200 projects, 80% in total)
//	  t00/g1/g2/.../g7 (group)      deep chain to depth 8, 5% in total
//	o1..o9 (other organizations)    each with a team and 3 projects, 5% in total
//
// Topics are drawn independently of scopes, so a filtered query's nearest
// neighbours are mostly outside the filter: the hard case for post-filtering.
func load(t *testing.T, ctx context.Context, pool *pgxpool.Pool, rng *rand.Rand, centroids [][]float32, n int) []scenario {
	t.Helper()
	svc := memory.NewService(pool)
	owner, err := db.New(pool).CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: "bench"})
	if err != nil {
		t.Fatal(err)
	}
	must := func(s memory.Scope, err error) memory.Scope {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	type weighted struct {
		scope  memory.Scope
		weight float64
	}
	var ws []weighted
	acme := must(svc.CreateOrganization(ctx, owner.ID, "acme", "Acme"))
	if _, err := svc.ConfigureOrg(ctx, "acme", []string{"organization", "team", "project", "group"}, 10); err != nil {
		t.Fatal(err)
	}
	ws = append(ws, weighted{acme, 0.02})
	var teams []memory.Scope
	var projects []memory.Scope
	for i := range 20 {
		tm := must(svc.CreateScope(ctx, uuid.Nil, "acme", "team", fmt.Sprintf("t%02d", i), ""))
		teams = append(teams, tm)
		ws = append(ws, weighted{tm, 0.004})
		for j := range 10 {
			p := must(svc.CreateScope(ctx, uuid.Nil, tm.Path, "project", fmt.Sprintf("p%d", j), ""))
			projects = append(projects, p)
			ws = append(ws, weighted{p, 0.004})
		}
	}
	chain := []memory.Scope{acme, teams[0]}
	parent := teams[0].Path
	for d := 1; d <= 7; d++ {
		g := must(svc.CreateScope(ctx, uuid.Nil, parent, "group", fmt.Sprintf("g%d", d), ""))
		chain = append(chain, g)
		ws = append(ws, weighted{g, 0.05 / 7})
		parent = g.Path
	}
	var small []memory.Scope
	for o := 1; o <= 9; o++ {
		org := must(svc.CreateOrganization(ctx, owner.ID, fmt.Sprintf("o%d", o), fmt.Sprintf("Org %d", o)))
		tm := must(svc.CreateScope(ctx, uuid.Nil, org.Path, "team", "team", ""))
		ws = append(ws, weighted{org, 0.05 / 45}, weighted{tm, 0.05 / 45})
		if o == 1 {
			small = append(small, org, tm)
		}
		for j := range 3 {
			p := must(svc.CreateScope(ctx, uuid.Nil, tm.Path, "project", fmt.Sprintf("p%d", j), ""))
			ws = append(ws, weighted{p, 0.05 / 45})
			if o == 1 && j == 0 {
				small = append(small, p)
			}
		}
	}

	// Assign memory counts per scope.
	count := map[uuid.UUID]int{}
	assigned := 0
	for _, w := range ws {
		c := int(w.weight * float64(n))
		count[w.scope.ID] = c
		assigned += c
	}
	for assigned < n { // rounding remainder goes to projects
		count[projects[rng.IntN(len(projects))].ID]++
		assigned++
	}

	type row struct {
		id, org, scope uuid.UUID
		vec            []float32
	}
	rows := make([]row, 0, n)
	for _, w := range ws {
		for range count[w.scope.ID] {
			rows = append(rows, row{id: uuid.New(), org: w.scope.OrgID, scope: w.scope.ID,
				vec: randomUnit(rng, centroids[rng.IntN(topics)], 0.9)})
		}
	}
	copyText(t, ctx, pool, "COPY memories (id, org_id, scope_id, kind, status, trust, current_revision, created_by) FROM STDIN",
		func(w *bufio.Writer) {
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\tfact\tactive\tunverified\t1\t%s\n", r.id, r.org, r.scope, owner.ID)
			}
		})
	copyText(t, ctx, pool, "COPY memory_revisions (memory_id, revision, org_id, scope_id, kind, title, content, changed_by) FROM STDIN",
		func(w *bufio.Writer) {
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t1\t%s\t%s\tfact\tsynthetic\tsynthetic memory for the vector benchmark\t%s\n", r.id, r.org, r.scope, owner.ID)
			}
		})
	copyText(t, ctx, pool, "COPY memory_embeddings (memory_id, revision, org_id, scope_id, model, dimensions, embedding) FROM STDIN",
		func(w *bufio.Writer) {
			buf := make([]byte, 0, 32)
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t1\t%s\t%s\t%s\t%d\t[", r.id, r.org, r.scope, model.Name, dims)
				for i, x := range r.vec {
					if i > 0 {
						_ = w.WriteByte(',')
					}
					_, _ = w.Write(strconv.AppendFloat(buf[:0], float64(x), 'g', -1, 32))
				}
				_, _ = w.WriteString("]\n")
			}
		})

	size := func(scopes ...memory.Scope) (ids []uuid.UUID, rows int) {
		for _, s := range scopes {
			ids = append(ids, s.ID)
			rows += count[s.ID]
		}
		return ids, rows
	}
	mk := func(name string, scopes ...memory.Scope) scenario {
		ids, r := size(scopes...)
		return scenario{Name: name, ScopeIDs: ids, Rows: r}
	}
	var everything []memory.Scope
	for _, w := range ws {
		everything = append(everything, w.scope)
	}
	return []scenario{
		mk("org root (depth 0)", acme),
		mk("team (depth 1)", acme, teams[1]),
		mk("project (depth 2)", acme, teams[1], projects[13]),
		mk("deep leaf (depth 8)", chain...),
		mk("small-org project", small...),
		mk("unfiltered reference", everything...),
	}
}

// copyText streams COPY text format rows produced by write.
func copyText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, write func(*bufio.Writer)) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	pr, pw := io.Pipe()
	go func() {
		bw := bufio.NewWriterSize(pw, 1<<20)
		write(bw)
		_ = pw.CloseWithError(bw.Flush())
	}()
	if _, err := conn.Conn().PgConn().CopyFrom(ctx, pr, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}
