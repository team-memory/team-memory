package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/jobs"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// JobKind is the jobs.kind value of embedding jobs.
const JobKind = "embed"

// KeyPrefix is the dedupe-key prefix of a model's embedding jobs. Keys are
// embed:<model>|<memory>:<revision>; '|' cannot occur in a model name, so a
// model's prefix never matches another model's keys (nomic-embed-text vs
// nomic-embed-text:v1.5).
func KeyPrefix(model string) string { return "embed:" + model + "|" }

type jobPayload struct {
	MemoryID uuid.UUID `json:"memory_id"`
	Revision int32     `json:"revision"`
	Model    string    `json:"model"`
}

// Indexer is the embedding job handler for one model. Processing is
// idempotent per (memory_id, revision, model): an existing embedding is never
// recomputed, and a job for a revision that is no longer current succeeds
// without work because the newer revision has its own job.
type Indexer struct {
	pool     *pgxpool.Pool
	provider EmbeddingProvider
	model    Model
	// ConflictThreshold is the cosine similarity at or above which a pair of
	// same-kind memories in ancestor/descendant scopes is recorded as a
	// conflict candidate. Zero disables detection.
	ConflictThreshold float64
	Logger            *slog.Logger
}

// NewIndexer checks that the provider's model is registered with the same
// dimensions, and fails loudly otherwise.
func NewIndexer(ctx context.Context, pool *pgxpool.Pool, p EmbeddingProvider) (*Indexer, error) {
	m := p.Model()
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := CheckRegistered(ctx, pool, m); err != nil {
		return nil, err
	}
	return &Indexer{pool: pool, provider: p, model: m, Logger: slog.New(slog.DiscardHandler)}, nil
}

// CheckRegistered verifies the model exists in embedding_models, enabled,
// with the configured dimensions.
func CheckRegistered(ctx context.Context, pool *pgxpool.Pool, m Model) error {
	reg, err := db.New(pool).GetEmbeddingModel(ctx, m.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("embedding model %q is not registered; run `team-memory embeddings add-model --name %s --dimensions %d`",
			m.Name, m.Name, m.Dimensions)
	}
	if err != nil {
		return err
	}
	if int(reg.Dimensions) != m.Dimensions {
		return fmt.Errorf("%w: model %s is registered with %d dimensions but configured with %d",
			ErrDimensionMismatch, m.Name, reg.Dimensions, m.Dimensions)
	}
	if !reg.Enabled {
		// New revisions get no jobs for a disabled model, so serving it would
		// silently drop them from vector retrieval.
		return fmt.Errorf("embedding model %q is disabled; configure an enabled model or run `team-memory embeddings enable %s`", m.Name, m.Name)
	}
	return nil
}

// Probe embeds a short text to check that the provider is reachable and
// returns the configured dimensions.
func Probe(ctx context.Context, p EmbeddingProvider) error {
	_, err := EmbedQuery(ctx, p, "dimension probe")
	return err
}

// Backfill enqueues jobs for current revisions that have no embedding for
// this model, e.g. memories written before the model was registered.
func (ix *Indexer) Backfill(ctx context.Context) (int64, error) {
	return db.New(ix.pool).BackfillEmbedJobs(ctx, ix.model.Name)
}

// Worker returns a jobs worker that processes only this model's jobs.
func (ix *Indexer) Worker(cfg jobs.Config) *jobs.Worker {
	cfg.KeyPrefix = KeyPrefix(ix.model.Name)
	return jobs.NewWorker(ix.pool, JobKind, ix.Handle, cfg)
}

type pending struct {
	idx int
	rev db.GetRevisionForEmbeddingRow
}

// Handle processes a batch of embedding jobs with one provider call.
func (ix *Indexer) Handle(ctx context.Context, batch []jobs.Job) []jobs.Outcome {
	out := make([]jobs.Outcome, len(batch))
	q := db.New(ix.pool)
	var work []pending
	for i, j := range batch {
		var p jobPayload
		if err := json.Unmarshal(j.Payload, &p); err != nil || p.MemoryID == uuid.Nil || p.Revision < 1 {
			out[i] = jobs.Outcome{Err: fmt.Errorf("invalid embed payload %s", j.Payload), Permanent: true}
			continue
		}
		if p.Model != ix.model.Name {
			out[i] = jobs.Outcome{Err: fmt.Errorf("job for model %q claimed by %q worker", p.Model, ix.model.Name), Permanent: true}
			continue
		}
		rev, err := q.GetRevisionForEmbedding(ctx, db.GetRevisionForEmbeddingParams{MemoryID: p.MemoryID, Revision: p.Revision})
		if errors.Is(err, pgx.ErrNoRows) {
			out[i] = jobs.Outcome{Err: fmt.Errorf("revision %s@%d does not exist", p.MemoryID, p.Revision), Permanent: true}
			continue
		}
		if err != nil {
			out[i] = jobs.Outcome{Err: err}
			continue
		}
		if rev.CurrentRevision != rev.Revision || rev.Status == "archived" {
			continue // superseded by a newer revision's job, or not searchable
		}
		exists, err := q.EmbeddingExists(ctx, db.EmbeddingExistsParams{MemoryID: p.MemoryID, Revision: p.Revision, Model: ix.model.Name})
		if err != nil {
			out[i] = jobs.Outcome{Err: err}
			continue
		}
		if exists {
			continue
		}
		work = append(work, pending{idx: i, rev: rev})
	}
	if len(work) == 0 {
		return out
	}
	texts := make([]string, len(work))
	for i, w := range work {
		texts[i] = DocumentText(w.rev.Title, w.rev.Content, w.rev.Rationale)
	}
	vecs, err := EmbedDocuments(ctx, ix.provider, texts)
	if err != nil {
		permanent := errors.Is(err, ErrDimensionMismatch)
		for _, w := range work {
			out[w.idx] = jobs.Outcome{Err: err, Permanent: permanent}
		}
		return out
	}
	for k, w := range work {
		out[w.idx] = jobs.Outcome{Err: ix.store(ctx, w.rev, vecs[k])}
	}
	return out
}

// store writes the embedding, drops older revisions' embeddings for the
// model, and records conflict candidates, in one transaction.
func (ix *Indexer) store(ctx context.Context, r db.GetRevisionForEmbeddingRow, vec []float32) error {
	return postgres.WithTx(ctx, ix.pool, uuid.Nil, func(tx postgres.Tx) error {
		// The provider call ran outside any transaction, so a newer revision
		// may have been committed meanwhile. Recheck under a share lock, which
		// also keeps a new revision from committing until this one is stored.
		cur, err := tx.Q.LockMemoryRevision(ctx, r.MemoryID)
		if err != nil {
			return err
		}
		if cur.CurrentRevision != r.Revision || cur.Status == "archived" {
			return nil
		}
		r.Status = cur.Status
		if _, err := tx.Q.InsertEmbedding(ctx, db.InsertEmbeddingParams{
			MemoryID: r.MemoryID, Revision: r.Revision, OrgID: r.OrgID, ScopeID: r.ScopeID,
			Model: ix.model.Name, Dimensions: int32(ix.model.Dimensions), Embedding: pgvector.NewVector(vec),
		}); err != nil {
			return fmt.Errorf("insert embedding: %w", err)
		}
		if _, err := tx.Q.DeleteOlderEmbeddings(ctx, db.DeleteOlderEmbeddingsParams{
			MemoryID: r.MemoryID, Model: ix.model.Name, Revision: r.Revision,
		}); err != nil {
			return err
		}
		if ix.ConflictThreshold <= 0 || r.Status != "active" {
			return nil
		}
		return ix.detectConflicts(ctx, tx, r, vec)
	})
}

// detectConflicts records same-kind, highly similar memories in strict
// ancestor or descendant scopes. Siblings are skipped: no reader sees both.
// Candidates are suggestions for the review queue (stage 05); they never
// change search results.
func (ix *Indexer) detectConflicts(ctx context.Context, tx postgres.Tx, r db.GetRevisionForEmbeddingRow, vec []float32) error {
	ancestors, err := tx.Q.ListAncestors(ctx, r.ScopeID)
	if err != nil {
		return err
	}
	isAncestor := map[uuid.UUID]bool{}
	for _, a := range ancestors {
		if a.Distance > 0 {
			isAncestor[a.ID] = true
		}
	}
	related, err := tx.Q.RelatedScopes(ctx, r.ScopeID)
	if err != nil || len(related) == 0 {
		return err
	}
	matches, err := Neighbors(ctx, tx, ix.model, vec, NeighborFilter{
		ScopeIDs: related, Kind: r.Kind, ExcludeID: r.MemoryID, MinSimilarity: ix.ConflictThreshold, Limit: 10,
	})
	if err != nil {
		return err
	}
	for _, n := range matches {
		p := db.InsertConflictCandidateParams{OrgID: r.OrgID, Model: ix.model.Name, Similarity: float32(n.Similarity)}
		if isAncestor[n.ScopeID] {
			// The new memory is the more specific one.
			p.ScopeID, p.MemoryID, p.Revision = r.ScopeID, r.MemoryID, r.Revision
			p.CandidateScopeID, p.CandidateMemoryID, p.CandidateRevision = n.ScopeID, n.MemoryID, n.Revision
		} else {
			p.ScopeID, p.MemoryID, p.Revision = n.ScopeID, n.MemoryID, n.Revision
			p.CandidateScopeID, p.CandidateMemoryID, p.CandidateRevision = r.ScopeID, r.MemoryID, r.Revision
		}
		if err := tx.Q.InsertConflictCandidate(ctx, p); err != nil {
			return fmt.Errorf("insert conflict candidate: %w", err)
		}
	}
	return nil
}

// NeighborFilter restricts a nearest-neighbour query.
type NeighborFilter struct {
	ScopeIDs      []uuid.UUID
	Kind          string // empty: any kind
	ExcludeID     uuid.UUID
	MinSimilarity float64
	Limit         int
}

// Neighbor is a memory whose current, active revision is close to a vector.
type Neighbor struct {
	MemoryID   uuid.UUID
	Revision   int32
	ScopeID    uuid.UUID
	Similarity float64
}

// Neighbors returns active memories in the given scopes ordered by cosine
// similarity to vec, using the model's own vectors only.
func Neighbors(ctx context.Context, tx pgx.Tx, m Model, vec []float32, f NeighborFilter) ([]Neighbor, error) {
	if err := ApplyVectorSettings(ctx, tx, "relaxed_order", 100); err != nil {
		return nil, err
	}
	sql := fmt.Sprintf(`SELECT e.memory_id, e.revision, e.scope_id, 1 - (%[1]s <=> %[2]s) AS similarity
FROM memory_embeddings e
JOIN memories mem ON mem.id = e.memory_id AND mem.current_revision = e.revision AND mem.scope_id = e.scope_id
WHERE %[3]s AND e.scope_id = ANY($2) AND mem.status = 'active'
  AND ($3 = '' OR mem.kind = $3) AND e.memory_id <> $4
ORDER BY %[1]s <=> %[2]s
LIMIT $5`, m.VectorExpr("e"), m.ParamExpr(1), m.Predicate("e"))
	rows, err := tx.Query(ctx, sql, pgvector.NewVector(vec), f.ScopeIDs, f.Kind, f.ExcludeID, f.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Neighbor
	for rows.Next() {
		var n Neighbor
		if err := rows.Scan(&n.MemoryID, &n.Revision, &n.ScopeID, &n.Similarity); err != nil {
			return nil, err
		}
		if n.Similarity >= f.MinSimilarity {
			out = append(out, n)
		}
	}
	return out, rows.Err()
}
