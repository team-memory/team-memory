// Package eval measures retrieval quality on the labeled synthetic set in
// dataset.json against a real embedding model. It backs
// docs/eval/retrieval-report.md and runs only when configured:
//
//	TEAM_MEMORY_OLLAMA_URL=https://ollama.example.com \
//	TEAM_MEMORY_EVAL_MODELS=nomic-embed-text,qwen3-embedding:0.6b \
//	make eval
//
// TEAM_MEMORY_EVAL_CONFLICT sets the conflict threshold the worker uses while
// embedding (default: the model's built-in default), so the stored
// candidates can be checked against the offline sweep. Results are written to
// TEAM_MEMORY_EVAL_OUT (default: this directory) as results-<model>.json.
package eval

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
	"github.com/team-memory/team-memory/internal/testdb"
)

type dataset struct {
	Scopes   []string `json:"scopes"`
	Memories []struct {
		Key       string `json:"key"`
		Scope     string `json:"scope"`
		Kind      string `json:"kind"`
		Title     string `json:"title"`
		Content   string `json:"content"`
		Rationale string `json:"rationale"`
		SourceURI string `json:"source_uri"`
		Approved  bool   `json:"approved"`
	} `json:"memories"`
	Queries []struct {
		ID       string   `json:"id"`
		Category string   `json:"category"`
		Scope    string   `json:"scope"`
		Query    string   `json:"query"`
		Relevant []string `json:"relevant"`
	} `json:"queries"`
	Unanswerable []struct {
		ID    string `json:"id"`
		Scope string `json:"scope"`
		Query string `json:"query"`
	} `json:"unanswerable"`
	Conflicts     [][2]string `json:"conflicts"`
	HardNegatives [][2]string `json:"hard_negatives"`
}

// Metrics aggregates one retrieval mode.
type Metrics struct {
	Queries    int                `json:"queries"`
	RecallAt5  float64            `json:"recall_at_5"`
	MRR        float64            `json:"mrr_at_10"`
	ByCategory map[string]float64 `json:"recall_at_5_by_category"`
	MRRByCat   map[string]float64 `json:"mrr_by_category"`
	Misses     []string           `json:"misses"` // queries with recall@5 < 1
	P50ms      float64            `json:"p50_ms"`
}

// ThresholdRow is one MinVectorSimilarity setting.
type ThresholdRow struct {
	MinSimilarity        float64 `json:"min_similarity"`
	RecallAt5            float64 `json:"hybrid_recall_at_5"`
	MRR                  float64 `json:"hybrid_mrr_at_10"`
	UnanswerableResults  float64 `json:"unanswerable_mean_results"`
	UnanswerableVecOnly  float64 `json:"unanswerable_mean_vector_only_results"`
	AnswerableMeanResult float64 `json:"answerable_mean_results"`
}

// ConflictRow is one conflict-detector threshold.
type ConflictRow struct {
	Threshold float64 `json:"threshold"`
	Detected  int     `json:"detected"`
	TruePos   int     `json:"true_positives"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
}

// PairSim reports a labeled pair's similarity.
type PairSim struct {
	Deeper     string  `json:"deeper"`
	Ancestor   string  `json:"ancestor"`
	SameKind   bool    `json:"same_kind"`
	Similarity float64 `json:"similarity"`
}

// Report is everything measured for one model.
type Report struct {
	Model            string             `json:"model"`
	Dimensions       int                `json:"dimensions"`
	Memories         int                `json:"memories"`
	EmbedSeconds     float64            `json:"embed_seconds"`
	Modes            map[string]Metrics `json:"modes"`
	Thresholds       []ThresholdRow     `json:"min_similarity_sweep"`
	RelevantSimMin   float64            `json:"relevant_similarity_min"`
	RelevantSimP10   float64            `json:"relevant_similarity_p10"`
	UnanswerableTop  []float64          `json:"unanswerable_top_similarity"`
	Conflict         []ConflictRow      `json:"conflict_sweep"`
	ConflictPairs    []PairSim          `json:"labeled_pairs"`
	CandidatePairs   int                `json:"same_kind_ancestor_pairs"`
	StoredThreshold  float64            `json:"stored_conflict_threshold"`
	StoredCandidates int                `json:"stored_conflict_candidates"`
	StoredMatches    bool               `json:"stored_matches_offline"`
}

func TestRetrievalEval(t *testing.T) {
	url, models := os.Getenv("TEAM_MEMORY_OLLAMA_URL"), os.Getenv("TEAM_MEMORY_EVAL_MODELS")
	if url == "" || models == "" {
		t.Skip("set TEAM_MEMORY_OLLAMA_URL and TEAM_MEMORY_EVAL_MODELS to run the retrieval evaluation")
	}
	raw, err := os.ReadFile("dataset.json")
	if err != nil {
		t.Fatal(err)
	}
	var ds dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("TEAM_MEMORY_EVAL_OUT")
	if out == "" {
		out = "."
	}
	for _, name := range strings.Split(models, ",") {
		name = strings.TrimSpace(name)
		t.Run(name, func(t *testing.T) {
			rep := evaluate(t, url, name, &ds)
			b, _ := json.MarshalIndent(rep, "", "  ")
			file := filepath.Join(out, "results-"+strings.NewReplacer(":", "-", "/", "-").Replace(name)+".json")
			if err := os.WriteFile(file, b, 0o644); err != nil {
				t.Fatal(err)
			}
			logReport(t, rep)
		})
	}
}

type fixture struct {
	pool   *pgxpool.Pool
	svc    *memory.Service
	owner  memory.Principal
	ids    map[string]uuid.UUID
	keys   map[uuid.UUID]string
	scopes map[string]string // key -> scope path
	kinds  map[string]string
}

func evaluate(t *testing.T, url, name string, ds *dataset) Report {
	ctx := context.Background()
	defaults, ok := embeddings.KnownModel(name)
	if !ok {
		t.Fatalf("model %s has no built-in defaults; add it to embeddings.knownModels", name)
	}
	model := defaults.Model
	pool := testdb.New(t)
	if err := embeddings.AddModel(ctx, pool, model, false); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE embedding_models SET enabled = (name = $1)", name); err != nil {
		t.Fatal(err)
	}
	f := load(t, ctx, pool, ds)
	provider, err := embeddings.NewOllama(url, model, &http.Client{Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}

	rep := Report{Model: name, Dimensions: model.Dimensions, Memories: len(ds.Memories), Modes: map[string]Metrics{}}
	rep.StoredThreshold = defaults.ConflictSimilarity
	if v := os.Getenv("TEAM_MEMORY_EVAL_CONFLICT"); v != "" {
		rep.StoredThreshold, _ = strconv.ParseFloat(v, 64)
	}
	ix, err := embeddings.NewIndexer(ctx, pool, provider)
	if err != nil {
		t.Fatal(err)
	}
	ix.ConflictThreshold = rep.StoredThreshold
	w := ix.Worker(jobs.Config{Name: "eval", BatchSize: 8})
	start := time.Now()
	for {
		n, err := w.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
	}
	rep.EmbedSeconds = time.Since(start).Seconds()
	var notDone int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE status <> 'succeeded'").Scan(&notDone)
	if notDone != 0 {
		t.Fatalf("%d embedding jobs did not succeed", notDone)
	}

	base := search.Config{Provider: provider, QueryTimeout: time.Minute}
	srch := search.New(pool, base)
	for _, mode := range []search.Mode{search.ModeFullText, search.ModeVector, search.ModeHybrid} {
		rep.Modes[string(mode)] = runMode(t, f, srch, ds, mode)
	}
	rep.Modes["fulltext_stage01_and"] = runStrictFTS(t, f, ds)

	// Similarity floor: relevant vs unanswerable similarities, then a sweep.
	var relSims []float64
	vec := search.New(pool, base)
	for _, q := range ds.Queries {
		res, err := vec.Search(ctx, f.owner, search.Input{Scope: q.Scope, Query: q.Query, Mode: search.ModeVector, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res.Results {
			if slices.Contains(q.Relevant, f.keys[r.ID]) {
				relSims = append(relSims, *r.VectorSimilarity)
			}
		}
	}
	slices.Sort(relSims)
	rep.RelevantSimMin, rep.RelevantSimP10 = relSims[0], relSims[len(relSims)/10]
	for _, q := range ds.Unanswerable {
		res, _ := vec.Search(ctx, f.owner, search.Input{Scope: q.Scope, Query: q.Query, Mode: search.ModeVector, Limit: 1})
		if len(res.Results) > 0 {
			rep.UnanswerableTop = append(rep.UnanswerableTop, round(*res.Results[0].VectorSimilarity))
		}
	}
	for _, th := range []float64{0, 0.4, 0.45, 0.5, 0.55, 0.6, 0.65, 0.7, 0.75} {
		cfg := base
		cfg.MinVectorSimilarity = th
		s := search.New(pool, cfg)
		m := runMode(t, f, s, ds, search.ModeHybrid)
		row := ThresholdRow{MinSimilarity: th, RecallAt5: m.RecallAt5, MRR: m.MRR}
		var total, vecOnly, ans float64
		for _, q := range ds.Unanswerable {
			res, _ := s.Search(ctx, f.owner, search.Input{Scope: q.Scope, Query: q.Query})
			total += float64(len(res.Results))
			for _, r := range res.Results {
				if slices.Equal(r.Retrievers, []string{search.RetrieverVector}) {
					vecOnly++
				}
			}
		}
		for _, q := range ds.Queries {
			res, _ := s.Search(ctx, f.owner, search.Input{Scope: q.Scope, Query: q.Query})
			ans += float64(len(res.Results))
		}
		row.UnanswerableResults = total / float64(len(ds.Unanswerable))
		row.UnanswerableVecOnly = vecOnly / float64(len(ds.Unanswerable))
		row.AnswerableMeanResult = ans / float64(len(ds.Queries))
		rep.Thresholds = append(rep.Thresholds, row)
	}

	evalConflicts(t, ctx, f, ds, model, &rep)
	return rep
}

func load(t *testing.T, ctx context.Context, pool *pgxpool.Pool, ds *dataset) *fixture {
	svc := memory.NewService(pool)
	p, err := db.New(pool).CreatePrincipal(ctx, db.CreatePrincipalParams{Kind: "human", DisplayName: "Eval"})
	if err != nil {
		t.Fatal(err)
	}
	owner := memory.Principal{ID: p.ID, Kind: p.Kind, DisplayName: p.DisplayName}
	levels := []string{"organization", "team", "project"}
	for _, path := range ds.Scopes {
		parts := strings.Split(path, "/")
		if len(parts) == 1 {
			if _, err := svc.CreateOrganization(ctx, owner.ID, path, path); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := svc.CreateScope(ctx, uuid.Nil, strings.Join(parts[:len(parts)-1], "/"), levels[len(parts)-1], parts[len(parts)-1], ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.AddMember(ctx, uuid.Nil, ds.Scopes[0], owner.ID, memory.RoleMaintainer, true); err != nil {
		t.Fatal(err)
	}
	f := &fixture{pool: pool, svc: svc, owner: owner, ids: map[string]uuid.UUID{}, keys: map[uuid.UUID]string{},
		scopes: map[string]string{}, kinds: map[string]string{}}
	for _, m := range ds.Memories {
		mem, err := svc.Create(ctx, owner, memory.CreateInput{Scope: m.Scope, Kind: m.Kind, Title: m.Title,
			Content: m.Content, Rationale: m.Rationale, SourceURI: m.SourceURI})
		if err != nil {
			t.Fatalf("%s: %v", m.Key, err)
		}
		f.ids[m.Key], f.keys[mem.ID], f.scopes[m.Key], f.kinds[m.Key] = mem.ID, m.Key, m.Scope, m.Kind
		if m.Approved {
			approve(t, ctx, pool, owner.ID, mem.ID)
		}
	}
	return f
}

// approve records an approval directly, standing in for stage 05's browser
// review, so trust boosts are exercised.
func approve(t *testing.T, ctx context.Context, pool *pgxpool.Pool, actor, id uuid.UUID) {
	var rev int32
	var org, scope uuid.UUID
	if err := pool.QueryRow(ctx, `UPDATE memories SET trust = 'approved', state_version = state_version + 1
		WHERE id = $1 RETURNING current_revision, org_id, scope_id`, id).Scan(&rev, &org, &scope); err != nil {
		t.Fatal(err)
	}
	ev, err := db.New(pool).InsertMemoryEvent(ctx, db.InsertMemoryEventParams{OrgID: org, ScopeID: scope, MemoryID: id,
		Revision: rev, EventType: "approved", ActorID: actor, Snapshot: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO approvals (org_id, memory_id, revision, event_id, decision, reviewer_id)
		VALUES ($1, $2, $3, $4, 'approved', $5)`, org, id, rev, ev.ID, actor); err != nil {
		t.Fatal(err)
	}
}

type scored struct {
	recall5, rr float64
}

func score(ranked []string, relevant []string) scored {
	var s scored
	hits := 0
	for i, k := range ranked {
		if !slices.Contains(relevant, k) {
			continue
		}
		if i < 5 {
			hits++
		}
		if s.rr == 0 && i < 10 {
			s.rr = 1 / float64(i+1)
		}
	}
	s.recall5 = float64(hits) / float64(len(relevant))
	return s
}

func aggregate(ds *dataset, per map[string]scored, lat []float64) Metrics {
	m := Metrics{Queries: len(ds.Queries), ByCategory: map[string]float64{}, MRRByCat: map[string]float64{}}
	counts := map[string]float64{}
	for _, q := range ds.Queries {
		s := per[q.ID]
		m.RecallAt5 += s.recall5
		m.MRR += s.rr
		m.ByCategory[q.Category] += s.recall5
		m.MRRByCat[q.Category] += s.rr
		counts[q.Category]++
		if s.recall5 < 1 {
			m.Misses = append(m.Misses, q.ID)
		}
	}
	n := float64(len(ds.Queries))
	m.RecallAt5, m.MRR = round(m.RecallAt5/n), round(m.MRR/n)
	for c, k := range counts {
		m.ByCategory[c] = round(m.ByCategory[c] / k)
		m.MRRByCat[c] = round(m.MRRByCat[c] / k)
	}
	if len(lat) > 0 {
		slices.Sort(lat)
		m.P50ms = round(lat[len(lat)/2])
	}
	return m
}

func runMode(t *testing.T, f *fixture, s *search.Service, ds *dataset, mode search.Mode) Metrics {
	per := map[string]scored{}
	var lat []float64
	for _, q := range ds.Queries {
		start := time.Now()
		res, err := s.Search(context.Background(), f.owner, search.Input{Scope: q.Scope, Query: q.Query, Mode: mode, Limit: 10})
		if err != nil {
			t.Fatalf("%s %s: %v", mode, q.ID, err)
		}
		lat = append(lat, float64(time.Since(start).Microseconds())/1000)
		if mode != search.ModeFullText && res.Meta.Vector.Status != search.VectorOK {
			t.Fatalf("%s %s: vector %+v", mode, q.ID, res.Meta.Vector)
		}
		var ranked []string
		for _, r := range res.Results {
			ranked = append(ranked, f.keys[r.ID])
		}
		per[q.ID] = score(ranked, q.Relevant)
	}
	return aggregate(ds, per, lat)
}

// runStrictFTS is the stage 01 full-text behaviour (websearch_to_tsquery, all
// terms required) for comparison with the OR-ed lexemes stage 02 uses.
func runStrictFTS(t *testing.T, f *fixture, ds *dataset) Metrics {
	ctx := context.Background()
	per := map[string]scored{}
	for _, q := range ds.Queries {
		vs, err := f.svc.VisibleScopes(ctx, f.owner, q.Scope)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, len(vs))
		for i, v := range vs {
			ids[i] = v.ID
		}
		rows, err := db.New(f.pool).FullTextCandidates(ctx, db.FullTextCandidatesParams{Websearch: true, Query: q.Query,
			ScopeIds: ids, Statuses: []string{"active"}, Kinds: []string{}, ExcludeIds: []uuid.UUID{}, MaxResults: 10})
		if err != nil {
			t.Fatal(err)
		}
		var ranked []string
		for _, r := range rows {
			ranked = append(ranked, f.keys[r.ID])
		}
		per[q.ID] = score(ranked, q.Relevant)
	}
	return aggregate(ds, per, nil)
}

func isAncestor(anc, desc string) bool { return strings.HasPrefix(desc, anc+"/") }

func evalConflicts(t *testing.T, ctx context.Context, f *fixture, ds *dataset, m embeddings.Model, rep *Report) {
	vecs := map[string][]float32{}
	rows, err := f.pool.Query(ctx, "SELECT memory_id, embedding FROM memory_embeddings WHERE model = $1", m.Name)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		var v pgvector.Vector
		if err := rows.Scan(&id, &v); err != nil {
			t.Fatal(err)
		}
		vecs[f.keys[id]] = v.Slice()
	}
	rows.Close()
	cos := func(a, b []float32) float64 {
		var dot, na, nb float64
		for i := range a {
			dot += float64(a[i]) * float64(b[i])
			na += float64(a[i]) * float64(a[i])
			nb += float64(b[i]) * float64(b[i])
		}
		return dot / math.Sqrt(na*nb)
	}
	truth := map[[2]string]bool{}
	for _, c := range ds.Conflicts {
		truth[c] = true
	}
	type pair struct {
		key [2]string
		sim float64
	}
	var pairs []pair
	for _, d := range ds.Memories {
		for _, a := range ds.Memories {
			if d.Kind == a.Kind && isAncestor(a.Scope, d.Scope) {
				pairs = append(pairs, pair{[2]string{d.Key, a.Key}, cos(vecs[d.Key], vecs[a.Key])})
			}
		}
	}
	rep.CandidatePairs = len(pairs)
	for _, lp := range append(slices.Clone(ds.Conflicts), ds.HardNegatives...) {
		rep.ConflictPairs = append(rep.ConflictPairs, PairSim{Deeper: lp[0], Ancestor: lp[1],
			SameKind: f.kinds[lp[0]] == f.kinds[lp[1]], Similarity: round(cos(vecs[lp[0]], vecs[lp[1]]))})
	}
	for th := 0.50; th <= 0.951; th += 0.05 {
		row := ConflictRow{Threshold: round(th)}
		for _, p := range pairs {
			if p.sim >= th {
				row.Detected++
				if truth[p.key] {
					row.TruePos++
				}
			}
		}
		if row.Detected > 0 {
			row.Precision = round(float64(row.TruePos) / float64(row.Detected))
		}
		row.Recall = round(float64(row.TruePos) / float64(len(ds.Conflicts)))
		rep.Conflict = append(rep.Conflict, row)
	}
	// The worker detected candidates while embedding; they must equal the
	// offline computation at the same threshold.
	if rep.StoredThreshold > 0 {
		stored, err := db.New(f.pool).ListConflictCandidates(ctx, m.Name)
		if err != nil {
			t.Fatal(err)
		}
		got := map[[2]string]bool{}
		for _, c := range stored {
			got[[2]string{f.keys[c.MemoryID], f.keys[c.CandidateMemoryID]}] = true
		}
		want := map[[2]string]bool{}
		for _, p := range pairs {
			if p.sim >= rep.StoredThreshold-1e-6 {
				want[p.key] = true
			}
		}
		rep.StoredCandidates = len(got)
		rep.StoredMatches = len(got) == len(want)
		for k := range want {
			if !got[k] {
				rep.StoredMatches = false
				t.Logf("worker missed candidate %v", k)
			}
		}
	}
}

func round(f float64) float64 { return math.Round(f*1000) / 1000 }

func logReport(t *testing.T, r Report) {
	t.Logf("model %s (%d dims): embedded %d memories in %.1fs", r.Model, r.Dimensions, r.Memories, r.EmbedSeconds)
	for _, mode := range []string{"fulltext_stage01_and", "fulltext", "vector", "hybrid"} {
		m := r.Modes[mode]
		t.Logf("  %-22s recall@5 %.3f  MRR %.3f  by category %v  misses %v  p50 %.0fms", mode, m.RecallAt5, m.MRR, m.ByCategory, m.Misses, m.P50ms)
	}
	t.Logf("  relevant similarity min %.3f p10 %.3f; unanswerable top similarity %v", r.RelevantSimMin, r.RelevantSimP10, r.UnanswerableTop)
	for _, th := range r.Thresholds {
		t.Logf("  min_sim %.2f: hybrid recall@5 %.3f MRR %.3f | unanswerable mean results %.1f (vector-only %.1f) | answerable mean results %.1f",
			th.MinSimilarity, th.RecallAt5, th.MRR, th.UnanswerableResults, th.UnanswerableVecOnly, th.AnswerableMeanResult)
	}
	for _, p := range r.ConflictPairs {
		t.Logf("  pair %-16s -> %-18s same_kind=%-5t sim %.3f", p.Deeper, p.Ancestor, p.SameKind, p.Similarity)
	}
	for _, c := range r.Conflict {
		t.Logf("  conflict threshold %.2f: detected %d, true %d, precision %.3f, recall %.3f", c.Threshold, c.Detected, c.TruePos, c.Precision, c.Recall)
	}
	t.Logf("  same-kind ancestor pairs: %d; stored candidates at %.2f: %d (matches offline: %t)",
		r.CandidatePairs, r.StoredThreshold, r.StoredCandidates, r.StoredMatches)
}
