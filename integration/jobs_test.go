package integration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres/db"
	"github.com/team-memory/team-memory/internal/search"
)

type jobRow struct {
	status    string
	attempts  int
	lastError string
	runAfter  time.Time
}

func embedJob(t *testing.T, tr *tree, id uuid.UUID, rev int) (jobRow, bool) {
	t.Helper()
	var j jobRow
	var lastErr *string
	err := tr.db.Admin.QueryRow(context.Background(),
		`SELECT status, attempts, last_error, run_after FROM jobs WHERE dedupe_key = $1`,
		embeddings.KeyPrefix("nomic-embed-text")+id.String()+":"+strconv.Itoa(rev)).Scan(&j.status, &j.attempts, &lastErr, &j.runAfter)
	if err != nil {
		return j, false
	}
	if lastErr != nil {
		j.lastError = *lastErr
	}
	return j, true
}

func vectorOf(dims int) pgvector.Vector {
	v := make([]float32, dims)
	v[0] = 1
	return pgvector.NewVector(v)
}

func embeddingRevisions(t *testing.T, tr *tree, id uuid.UUID) []int {
	t.Helper()
	rows, err := tr.db.Admin.Query(context.Background(),
		`SELECT revision FROM memory_embeddings WHERE memory_id = $1 ORDER BY revision`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var r int
		_ = rows.Scan(&r)
		out = append(out, r)
	}
	return out
}

// makeReady lets retried jobs run now instead of after their backoff.
func makeReady(t *testing.T, tr *tree) {
	t.Helper()
	if _, err := tr.db.Admin.Exec(context.Background(), `UPDATE jobs SET run_after = now() WHERE status = 'pending'`); err != nil {
		t.Fatal(err)
	}
}

func TestRevisionsEnqueueEmbeddingJobs(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "decision", "Ledger store", "payments ledger lives in PostgreSQL")
	if j, ok := embedJob(t, tr, m.ID, 1); !ok || j.status != "pending" {
		t.Fatalf("create should enqueue a pending job in the same transaction: %+v %v", j, ok)
	}
	// A failed write enqueues nothing: the outbox shares the transaction.
	if _, err := tr.svc.Create(ctx, tr.owner, memory.CreateInput{Scope: "acme/platform/payments", Kind: "fact",
		Title: "x", Content: "y", SourceURI: "ftp://nope"}); err == nil {
		t.Fatal("invalid create succeeded")
	}
	var total int
	_ = tr.db.Admin.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&total)
	if total != 1 {
		t.Fatalf("jobs after failed create: %d", total)
	}

	tr.drain(t)
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "succeeded" {
		t.Fatalf("job after drain: %+v", j)
	}
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 1 || got[0] != 1 {
		t.Fatalf("embeddings: %v", got)
	}

	// A new revision gets its own job; once embedded, the old vector is dropped.
	if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("ledger moved to PostgreSQL 17")}); err != nil {
		t.Fatal(err)
	}
	if j, ok := embedJob(t, tr, m.ID, 2); !ok || j.status != "pending" {
		t.Fatalf("update job: %+v", j)
	}
	tr.drain(t)
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 1 || got[0] != 2 {
		t.Fatalf("embeddings after update: %v", got)
	}

	// Supersede enqueues for the replacement.
	_, repl, err := tr.svc.Supersede(ctx, tr.owner, memory.SupersedeInput{ID: m.ID, ExpectedRevision: 2,
		Replacement: memory.CreateInput{Title: "Ledger store v2", Content: "ledger in PostgreSQL 18"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := embedJob(t, tr, repl.ID, 1); !ok {
		t.Fatal("supersede did not enqueue the replacement")
	}
}

func TestWorkerRetriesThenDeadLetters(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "decision", "Ledger store", "payments ledger lives in PostgreSQL")
	if _, err := tr.db.Admin.Exec(ctx, `UPDATE jobs SET max_attempts = 3`); err != nil {
		t.Fatal(err)
	}
	tr.embedder.SetFailure(errors.New("connection refused"))
	w := tr.indexer(t).Worker(jobs.Config{Name: "w1", BackoffBase: time.Hour, BackoffMax: 2 * time.Hour})

	for attempt := 1; attempt <= 3; attempt++ {
		if n, err := w.RunOnce(ctx); err != nil || n != 1 {
			t.Fatalf("attempt %d: claimed %d, %v", attempt, n, err)
		}
		j, _ := embedJob(t, tr, m.ID, 1)
		if j.attempts != attempt || !strings.Contains(j.lastError, "connection refused") {
			t.Fatalf("attempt %d: %+v", attempt, j)
		}
		if attempt < 3 {
			if j.status != "pending" || time.Until(j.runAfter) < 30*time.Minute {
				t.Fatalf("attempt %d should back off: %+v", attempt, j)
			}
			// Backed-off jobs are not claimed early.
			if n, _ := w.RunOnce(ctx); n != 0 {
				t.Fatal("claimed a job during its backoff")
			}
			makeReady(t, tr)
		} else if j.status != "dead" {
			t.Fatalf("after max attempts: %+v", j)
		}
	}

	// No data loss: the memory is intact and searchable through full-text.
	res, err := tr.search.Search(ctx, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "ledger"})
	if err != nil || len(res.Results) != 1 || res.Results[0].ID != m.ID {
		t.Fatalf("search with dead job: %v %+v", err, res)
	}
	if res.Meta.Mode != "fulltext_only" || res.Meta.Vector.Status != search.VectorUnavailable {
		t.Fatalf("meta should report the fallback: %+v", res.Meta)
	}

	// Recovery: requeue dead jobs once the provider is back.
	tr.embedder.SetFailure(nil)
	if n, err := db.New(tr.db.Admin).RetryDeadJobs(ctx, embeddings.JobKind); err != nil || n != 1 {
		t.Fatalf("retry dead: %d %v", n, err)
	}
	tr.drain(t)
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "succeeded" {
		t.Fatalf("after recovery: %+v", j)
	}
	res, _ = tr.search.Search(ctx, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "ledger"})
	if res.Meta.Mode != "hybrid" || res.Meta.Vector.PendingEmbeddings != 0 || len(res.Results[0].Retrievers) != 2 {
		t.Fatalf("after recovery: %+v %+v", res.Meta, res.Results)
	}
}

// TestOllamaStoppedDeadLettersAndSearchFallsBack uses the real Ollama client
// against a server that is then stopped.
func TestOllamaStoppedDeadLettersAndSearchFallsBack(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	ollama, err := embeddings.NewOllama(srv.URL, embeddings.Model{Name: "nomic-embed-text", Dimensions: 768}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.Close() // Ollama is down
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	if _, err := tr.db.Admin.Exec(ctx, `UPDATE jobs SET max_attempts = 2`); err != nil {
		t.Fatal(err)
	}
	ix, err := embeddings.NewIndexer(ctx, tr.db.Worker, ollama)
	if err != nil {
		t.Fatal(err)
	}
	w := ix.Worker(jobs.Config{Name: "w", BackoffBase: time.Millisecond, BackoffMax: time.Millisecond})
	for range 2 {
		makeReady(t, tr)
		if _, err := w.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "dead" || j.attempts != 2 {
		t.Fatalf("job with Ollama down: %+v", j)
	}
	srch := search.New(tr.pool, search.Config{Provider: ollama})
	res, err := srch.Search(ctx, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "refund"})
	if err != nil || len(res.Results) != 1 || res.Meta.Mode != "fulltext_only" || res.Meta.Vector.Status != search.VectorUnavailable {
		t.Fatalf("fallback: %v %+v", err, res)
	}
	if strings.Contains(res.Meta.Vector.Message, "127.0.0.1") {
		t.Fatalf("provider address leaked to the client: %q", res.Meta.Vector.Message)
	}
}

func TestQueryEmbeddingTimeoutFallsBack(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	slow, _ := embeddings.NewOllama(srv.URL, embeddings.Model{Name: "nomic-embed-text", Dimensions: 768}, nil)
	tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	srch := search.New(tr.pool, search.Config{Provider: slow, QueryTimeout: 50 * time.Millisecond})
	start := time.Now()
	res, err := srch.Search(ctx, tr.owner, search.Input{Scope: "acme/platform/payments", Query: "refund"})
	if err != nil || len(res.Results) != 1 || res.Meta.Vector.Status != search.VectorTimeout {
		t.Fatalf("timeout fallback: %v %+v", err, res.Meta)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("search waited %s for the provider", time.Since(start))
	}
}

func TestDimensionMismatchIsPermanent(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	tr.embedder.SetWrongDimensions(true)
	if _, err := tr.indexer(t).Worker(jobs.Config{Name: "w"}).RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "dead" || j.attempts != 1 || !strings.Contains(j.lastError, "dimension") {
		t.Fatalf("mismatch should dead-letter on the first attempt: %+v", j)
	}
	// Startup checks refuse a model registered with other dimensions.
	if _, err := embeddings.NewIndexer(ctx, tr.db.Worker, embeddings.NewFake("nomic-embed-text", 1024)); !errors.Is(err, embeddings.ErrDimensionMismatch) {
		t.Fatalf("indexer with wrong dimensions: %v", err)
	}
	if _, err := embeddings.NewIndexer(ctx, tr.db.Worker, embeddings.NewFake("unregistered", 8)); err == nil {
		t.Fatal("indexer for an unregistered model started")
	}
	// The database rejects vectors whose length disagrees with the registry.
	_, err := db.New(tr.db.Admin).InsertEmbedding(ctx, db.InsertEmbeddingParams{MemoryID: m.ID, Revision: 1, OrgID: tr.org.ID,
		ScopeID: tr.payments.ID, Model: "nomic-embed-text", Dimensions: 768, Embedding: vectorOf(1024)})
	if err == nil {
		t.Fatal("stored a 1024-dimension vector for a 768-dimension model")
	}
}

func TestEmbeddingIsIdempotent(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	q := db.New(tr.db.Admin)
	// Enqueueing the same revision again is a no-op.
	if err := q.EnqueueEmbedJobs(ctx, db.EnqueueEmbedJobsParams{OrgID: tr.org.ID, MemoryID: m.ID, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if n, _ := q.BackfillEmbedJobs(ctx, "nomic-embed-text"); n != 0 {
		t.Fatalf("backfill duplicated %d jobs", n)
	}
	var count int
	_ = tr.db.Admin.QueryRow(ctx, `SELECT count(*) FROM jobs`).Scan(&count)
	if count != 1 {
		t.Fatalf("jobs: %d", count)
	}
	ix := tr.indexer(t)
	job := []jobs.Job{{ID: 1, Kind: "embed", Attempt: 1, MaxAttempts: 10,
		Payload: []byte(`{"memory_id":"` + m.ID.String() + `","revision":1,"model":"nomic-embed-text"}`)}}
	for range 3 {
		if out := ix.Handle(ctx, job); out[0].Err != nil {
			t.Fatal(out[0].Err)
		}
	}
	if calls := tr.embedder.Calls(); calls != 1 {
		t.Fatalf("provider called %d times for one (memory, revision, model)", calls)
	}
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 1 {
		t.Fatalf("embeddings: %v", got)
	}

	// A job for a revision that is no longer current succeeds without work.
	if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("60 days")}); err != nil {
		t.Fatal(err)
	}
	bad := []jobs.Job{{ID: 2, Kind: "embed", Attempt: 1, MaxAttempts: 10,
		Payload: []byte(`{"memory_id":"` + uuid.Nil.String() + `","revision":1,"model":"nomic-embed-text"}`)}}
	if out := ix.Handle(ctx, bad); out[0].Err == nil || !out[0].Permanent {
		t.Fatalf("bad payload should fail permanently: %+v", out)
	}
	calls := tr.embedder.Calls()
	if out := ix.Handle(ctx, job); out[0].Err != nil || tr.embedder.Calls() != calls {
		t.Fatalf("stale revision: %+v, calls %d -> %d", out, calls, tr.embedder.Calls())
	}
	// Backfill now finds the new revision only (its job already exists).
	if n, _ := q.BackfillEmbedJobs(ctx, "nomic-embed-text"); n != 0 {
		t.Fatalf("backfill: %d", n)
	}
}

func TestBackfillCoversMemoriesWithoutJobs(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	// Memories written while no model is enabled get no jobs.
	if _, err := tr.db.Admin.Exec(ctx, `UPDATE embedding_models SET enabled = false`); err != nil {
		t.Fatal(err)
	}
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	if _, ok := embedJob(t, tr, m.ID, 1); ok {
		t.Fatal("job enqueued for a disabled model")
	}
	// A disabled model is neither served, backfilled nor indexed.
	if _, err := embeddings.NewIndexer(ctx, tr.db.Worker, tr.embedder); err == nil {
		t.Fatal("indexer started for a disabled model")
	}
	if n, _ := db.New(tr.db.Admin).BackfillEmbedJobs(ctx, "nomic-embed-text"); n != 0 {
		t.Fatalf("backfill enqueued %d jobs for a disabled model", n)
	}
	if _, err := tr.db.Admin.Exec(ctx, `UPDATE embedding_models SET enabled = true`); err != nil {
		t.Fatal(err)
	}
	n, err := tr.indexer(t).Backfill(ctx)
	if err != nil || n != 1 {
		t.Fatalf("backfill: %d %v", n, err)
	}
	tr.drain(t)
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 1 {
		t.Fatalf("embeddings: %v", got)
	}
}

func TestExpiredLeaseIsReleased(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	q := db.New(tr.db.Admin)
	claim := func(worker string) []db.ClaimJobsRow {
		t.Helper()
		rows, err := q.ClaimJobs(ctx, db.ClaimJobsParams{Worker: worker, Kind: "embed", MaxJobs: 10})
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	expire := func() {
		t.Helper()
		if _, err := tr.db.Admin.Exec(ctx, `UPDATE jobs SET locked_at = now() - interval '2 minutes'`); err != nil {
			t.Fatal(err)
		}
	}
	// A worker named "w" claims the job and stalls.
	first := claim("w")
	if len(first) != 1 || first[0].LeaseID == nil {
		t.Fatalf("claim: %+v", first)
	}
	w := tr.indexer(t).Worker(jobs.Config{Name: "w", Lease: time.Minute})
	if n, _ := w.RunOnce(ctx); n != 0 {
		t.Fatal("claimed a job that is already leased")
	}
	if err := w.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "running" {
		t.Fatalf("lease released early: %+v", j)
	}
	expire()
	if err := w.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "pending" || j.attempts != 1 {
		t.Fatalf("after reap: %+v", j)
	}
	// A new process with the same worker name claims it again.
	second := claim("w")
	if len(second) != 1 || *second[0].LeaseID == *first[0].LeaseID {
		t.Fatalf("second claim: %+v", second)
	}
	// The stalled claim's late outcomes are ignored even though the name matches.
	stale := *first[0].LeaseID
	if n, _ := q.RetryJob(ctx, db.RetryJobParams{ID: first[0].ID, LeaseID: stale, LastError: "late", DelaySeconds: 0}); n != 0 {
		t.Fatal("stale lease reset a job another claim owns")
	}
	if n, _ := q.CompleteJob(ctx, db.CompleteJobParams{ID: first[0].ID, LeaseID: stale}); n != 0 {
		t.Fatal("stale lease completed a job another claim owns")
	}
	if n, _ := q.DeadLetterJob(ctx, db.DeadLetterJobParams{ID: first[0].ID, LeaseID: stale, LastError: "late"}); n != 0 {
		t.Fatal("stale lease dead-lettered a job another claim owns")
	}
	if rows := claim("other"); len(rows) != 0 {
		t.Fatal("a third worker claimed a job that is still running")
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "running" || j.attempts != 2 {
		t.Fatalf("after stale outcomes: %+v", j)
	}
	// Once the second claim also expires, the job is processed normally.
	expire()
	if err := w.Reap(ctx); err != nil {
		t.Fatal(err)
	}
	tr.drain(t)
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "succeeded" || j.attempts != 3 {
		t.Fatalf("after drain: %+v", j)
	}
}

// TestModelsClaimOnlyTheirOwnJobs registers a second model whose name extends
// the default's ("nomic-embed-text:v1.5"); each worker must leave the other
// model's jobs alone.
func TestModelsClaimOnlyTheirOwnJobs(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	other := embeddings.NewFake("nomic-embed-text:v1.5", 768)
	if err := embeddings.AddModel(ctx, tr.db.Admin, other.Model(), false); err != nil {
		t.Fatal(err)
	}
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	tr.drain(t) // the default model's worker
	var otherStatus string
	key := embeddings.KeyPrefix("nomic-embed-text:v1.5") + m.ID.String() + ":1"
	if err := tr.db.Admin.QueryRow(ctx, `SELECT status FROM jobs WHERE dedupe_key = $1`, key).Scan(&otherStatus); err != nil || otherStatus != "pending" {
		t.Fatalf("other model's job after the default worker ran: %q %v", otherStatus, err)
	}
	ix, err := embeddings.NewIndexer(ctx, tr.db.Worker, other)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := ix.Worker(jobs.Config{Name: "v15"}).RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("v1.5 worker: %d %v", n, err)
	}
	var models []string
	rows, _ := tr.db.Admin.Query(ctx, `SELECT model FROM memory_embeddings WHERE memory_id = $1 ORDER BY model`, m.ID)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		models = append(models, s)
	}
	rows.Close()
	if strings.Join(models, ",") != "nomic-embed-text,nomic-embed-text:v1.5" {
		t.Fatalf("embeddings: %v", models)
	}
	// Distinct indexes, both valid, were built for the two spaces.
	var n int
	_ = tr.db.Admin.QueryRow(ctx, `SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname IN ($1, $2) AND i.indisvalid`, embeddings.Model{Name: "nomic-embed-text"}.IndexName(), other.Model().IndexName()).Scan(&n)
	if n != 2 {
		t.Fatalf("valid per-model indexes: %d", n)
	}
}

// TestNewRevisionDuringEmbeddingIsNotOverwritten commits revision 2 while
// revision 1 is at the provider; the late revision 1 vector must be dropped.
func TestNewRevisionDuringEmbeddingIsNotOverwritten(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	m := tr.create(t, tr.owner, "acme/platform/payments", "fact", "Refund window", "refunds are accepted for 30 days")
	tr.embedder.SetHook(func() {
		tr.embedder.SetHook(nil)
		if _, err := tr.svc.Update(ctx, tr.owner, memory.UpdateInput{ID: m.ID, ExpectedRevision: 1, Content: strp("60 days")}); err != nil {
			t.Error(err)
		}
	})
	w := tr.indexer(t).Worker(jobs.Config{Name: "w"})
	if n, err := w.RunOnce(ctx); err != nil || n != 1 {
		t.Fatalf("run: %d %v", n, err)
	}
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 0 {
		t.Fatalf("stale revision embedding stored: %v", got)
	}
	if j, _ := embedJob(t, tr, m.ID, 1); j.status != "succeeded" {
		t.Fatalf("stale job: %+v", j)
	}
	tr.drain(t)
	if got := embeddingRevisions(t, tr, m.ID); len(got) != 1 || got[0] != 2 {
		t.Fatalf("embeddings after drain: %v", got)
	}
}

func TestConcurrentWorkersClaimEachJobOnce(t *testing.T) {
	ctx := context.Background()
	tr := newTree(t)
	for i := range 40 {
		tr.create(t, tr.owner, "acme/platform/payments", "fact", "note "+strconv.Itoa(i%10), "content number "+strconv.Itoa(i%10))
	}
	ix := tr.indexer(t)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := ix.Worker(jobs.Config{Name: "w" + strconv.Itoa(i), BatchSize: 3})
			for {
				n, err := w.RunOnce(ctx)
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	var succeeded, attempts int
	_ = tr.db.Admin.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status = 'succeeded'), sum(attempts) FROM jobs`).Scan(&succeeded, &attempts)
	if succeeded != 40 || attempts != 40 {
		t.Fatalf("succeeded %d, attempts %d; want 40 and 40", succeeded, attempts)
	}
}
