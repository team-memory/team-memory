// Package search is hybrid retrieval over memories: PostgreSQL full-text and
// pgvector similarity, fused with Reciprocal Rank Fusion. It searches only the
// current revision of memories in the searcher's scope and its ancestors (the
// visible set), filters by scope before ranking, and degrades to full-text
// only when the query cannot be embedded.
package search

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/memory"
	"github.com/team-memory/team-memory/internal/postgres"
	"github.com/team-memory/team-memory/internal/postgres/db"
)

// Mode selects retrievers. Hybrid is the default; the single-retriever modes
// exist for evaluation.
type Mode string

const (
	ModeHybrid   Mode = "hybrid"
	ModeFullText Mode = "fulltext"
	ModeVector   Mode = "vector"
)

// Vector status values reported in Meta.
const (
	VectorOK          = "ok"
	VectorDisabled    = "disabled"    // no embedding provider configured
	VectorSkipped     = "skipped"     // full-text mode requested
	VectorTimeout     = "timeout"     // query embedding exceeded QueryTimeout
	VectorUnavailable = "unavailable" // provider error
	VectorMisconfig   = "misconfigured"
)

// Config configures a Service. Zero values take the defaults noted.
type Config struct {
	// Provider embeds queries. Nil means full-text only.
	Provider embeddings.EmbeddingProvider
	// QueryTimeout bounds query embedding (default 2s); on expiry the search
	// falls back to full-text.
	QueryTimeout time.Duration
	// CandidateLimit is how many candidates each retriever contributes to
	// fusion (default 50).
	CandidateLimit int
	// IterativeScan is pgvector's hnsw.iterative_scan for vector candidates
	// ("off", "relaxed_order", "strict_order"; default relaxed_order) and
	// EFSearch its hnsw.ef_search (default 100). See
	// docs/decisions/vector-index-strategy.md.
	IterativeScan string
	EFSearch      int
	// MinVectorSimilarity drops vector candidates below this cosine
	// similarity, so unrelated queries do not return arbitrary nearest
	// neighbours. It is model-specific; 0 keeps every candidate.
	MinVectorSimilarity float64
	Logger              *slog.Logger
}

// Service runs searches.
type Service struct {
	pool *pgxpool.Pool
	cfg  Config
}

// New returns a search service.
func New(pool *pgxpool.Pool, cfg Config) *Service {
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 2 * time.Second
	}
	if cfg.CandidateLimit <= 0 {
		cfg.CandidateLimit = 50
	}
	if cfg.IterativeScan == "" {
		cfg.IterativeScan = "relaxed_order"
	}
	if cfg.EFSearch <= 0 {
		cfg.EFSearch = 100
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{pool: pool, cfg: cfg}
}

// Input is a search request.
type Input struct {
	Scope string
	Query string
	// Kinds restricts results to memory kinds ("types" in the MCP contract).
	Kinds []string
	Limit int
	// IncludeSuperseded adds superseded memories. Archived memories are never
	// returned.
	IncludeSuperseded bool
	// IncludeOverridden keeps memories hidden by an effective override and
	// flags them with OverriddenBy.
	IncludeOverridden bool
	Mode              Mode
}

// Result is one ranked memory with provenance.
type Result struct {
	ID        uuid.UUID     `json:"id"`
	Revision  int           `json:"revision"`
	Kind      memory.Kind   `json:"kind"`
	Status    memory.Status `json:"status"`
	Trust     memory.Trust  `json:"trust"`
	Title     string        `json:"title"`
	Content   string        `json:"content"`
	Rationale string        `json:"rationale,omitempty"`
	SourceURI string        `json:"source_uri,omitempty"`
	CreatedBy uuid.UUID     `json:"created_by"`
	UpdatedAt time.Time     `json:"updated_at"`
	// ApprovedBy and ApprovedAt identify the maintainer review behind
	// trust=approved; absent otherwise.
	ApprovedBy  *uuid.UUID `json:"approved_by,omitempty"`
	ApprovedAt  *time.Time `json:"approved_at,omitempty"`
	OriginScope string     `json:"origin_scope"`
	Inherited   bool       `json:"inherited"`
	// Precedence is the origin scope's depth (0 = organization). It is scope
	// context for ordering, not evidence of conflict or authority.
	Precedence int `json:"precedence"`
	// Score is a monotonic 0..1 transform of the fused rank score. It is not
	// a calibrated probability of relevance.
	Score      float64  `json:"score"`
	Retrievers []string `json:"retrievers"`
	// VectorSimilarity is the cosine similarity to the query embedding when
	// the vector retriever returned this memory.
	VectorSimilarity *float64 `json:"vector_similarity,omitempty"`
	// OverriddenBy lists effective overriding memories (only present when
	// IncludeOverridden is set).
	OverriddenBy []uuid.UUID `json:"overridden_by,omitempty"`
}

// VectorInfo reports what happened to the vector retriever.
type VectorInfo struct {
	Status string `json:"status"`
	Model  string `json:"model,omitempty"`
	// Message explains a fallback. Provider errors are logged, not returned,
	// so internal hostnames never reach clients.
	Message string `json:"message,omitempty"`
	// PendingEmbeddings counts visible memories whose current revision has no
	// embedding yet; they can be found by full-text only.
	PendingEmbeddings int `json:"pending_embeddings"`
}

// Meta describes how a search ran.
type Meta struct {
	Mode   string     `json:"mode"` // hybrid | fulltext_only | vector_only
	Vector VectorInfo `json:"vector"`
	// OverridesInEffect counts memories in the reader's scope chain hidden by
	// an effective override, whether or not they would have matched this
	// query. With IncludeOverridden they are returned, flagged, instead.
	OverridesInEffect int    `json:"overrides_in_effect"`
	ScoreNote         string `json:"score_note"`
}

// Response is a search result page.
type Response struct {
	Results []Result `json:"results"`
	Meta    Meta     `json:"meta"`
}

const scoreNote = "score is a 0-1 rank-based relevance score (Reciprocal Rank Fusion with bounded trust boosts), comparable only within one response; it is not a calibrated probability"

// Search runs a hybrid search.
func (s *Service) Search(ctx context.Context, p memory.Principal, in Input) (Response, error) {
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" {
		return Response{}, &memory.ValidationError{Msg: "query is required"}
	}
	if n := len([]rune(in.Query)); n > memory.MaxQueryLen {
		return Response{}, &memory.ValidationError{Msg: fmt.Sprintf("query exceeds %d characters", memory.MaxQueryLen)}
	}
	kinds, err := memory.ParseKinds(in.Kinds)
	if err != nil {
		return Response{}, err
	}
	if in.Mode == "" {
		in.Mode = ModeHybrid
	}
	limit := memory.ClampLimit(in.Limit)
	statuses := []string{string(memory.StatusActive)}
	if in.IncludeSuperseded {
		statuses = append(statuses, string(memory.StatusSuperseded))
	}

	meta := Meta{ScoreNote: scoreNote}
	var qvec []float32
	switch {
	case in.Mode == ModeFullText:
		meta.Vector.Status = VectorSkipped
	case s.cfg.Provider == nil:
		meta.Vector.Status = VectorDisabled
		meta.Vector.Message = "no embedding provider is configured; results are full-text only"
	default:
		meta.Vector.Model = s.cfg.Provider.Model().Name
		qvec, meta.Vector.Status, meta.Vector.Message = s.embedQuery(ctx, in.Query)
	}
	if in.Mode == ModeVector && qvec == nil {
		return Response{}, fmt.Errorf("vector-only search needs a query embedding: %s", meta.Vector.Status)
	}
	useFTS := in.Mode != ModeVector
	retrievers := 0
	switch {
	case useFTS && qvec != nil:
		meta.Mode, retrievers = "hybrid", 2
	case useFTS:
		meta.Mode, retrievers = "fulltext_only", 1
	default:
		meta.Mode, retrievers = "vector_only", 1
	}

	var out []Result
	err = postgres.WithReadTx(ctx, s.pool, p.ID, func(tx postgres.Tx) error {
		vs, err := memory.VisibleScopesTx(ctx, tx.Q, p, in.Scope)
		if err != nil {
			return err
		}
		scopeIDs := make([]uuid.UUID, len(vs))
		inherited := make(map[uuid.UUID]bool, len(vs))
		for i, v := range vs {
			scopeIDs[i] = v.ID
			inherited[v.ID] = v.Inherited
		}

		// Effective overrides are rechecked on every search across the whole
		// chain, independent of whether the overriding memory matches.
		ovr, err := tx.Q.EffectiveOverrides(ctx, scopeIDs)
		if err != nil {
			return err
		}
		overriddenBy := map[uuid.UUID][]uuid.UUID{}
		for _, o := range ovr {
			overriddenBy[o.OverriddenMemoryID] = append(overriddenBy[o.OverriddenMemoryID], o.OverridingMemoryID)
		}
		meta.OverridesInEffect = len(overriddenBy)
		exclude := []uuid.UUID{}
		if !in.IncludeOverridden {
			for id := range overriddenBy {
				exclude = append(exclude, id)
			}
		}

		cands := map[uuid.UUID]*candidate{}
		get := func(id uuid.UUID) *candidate {
			c, ok := cands[id]
			if !ok {
				c = &candidate{id: id}
				cands[id] = c
			}
			return c
		}
		if useFTS {
			rows, err := tx.Q.FullTextCandidates(ctx, db.FullTextCandidatesParams{
				Websearch: hasWebsearchSyntax(in.Query), Query: in.Query, ScopeIds: scopeIDs, Statuses: statuses,
				Kinds: kinds, ExcludeIds: exclude, MaxResults: int32(s.cfg.CandidateLimit),
			})
			if err != nil {
				return err
			}
			for i, r := range rows {
				get(r.ID).ftsRank = i + 1
			}
		}
		if qvec != nil {
			hits, err := s.vectorCandidates(ctx, tx, qvec, scopeIDs, statuses, kinds, exclude)
			if err != nil {
				return err
			}
			for i, h := range hits {
				c := get(h.id)
				c.vecRank, c.vecSim = i+1, h.similarity
			}
			missing, err := tx.Q.CountMissingEmbeddings(ctx, db.CountMissingEmbeddingsParams{
				ScopeIds: scopeIDs, Statuses: statuses, Model: s.cfg.Provider.Model().Name,
			})
			if err != nil {
				return err
			}
			meta.Vector.PendingEmbeddings = int(missing)
		}
		if len(cands) == 0 {
			return nil
		}

		ids := make([]uuid.UUID, 0, len(cands))
		for id := range cands {
			ids = append(ids, id)
		}
		rows, err := tx.Q.SearchRows(ctx, ids)
		if err != nil {
			return err
		}
		byID := make(map[uuid.UUID]db.SearchRowsRow, len(rows))
		list := make([]*candidate, 0, len(rows))
		for _, r := range rows {
			byID[r.ID] = r
			c := cands[r.ID]
			c.kind, c.trust, c.depth = memory.Kind(r.Kind), memory.Trust(r.Trust), int(r.ScopeDepth)
			list = append(list, c)
		}
		ranked := rank(list, retrievers)
		for _, c := range ranked[:min(limit, len(ranked))] {
			r := byID[c.id]
			res := Result{
				ID: r.ID, Revision: int(r.CurrentRevision), Kind: memory.Kind(r.Kind), Status: memory.Status(r.Status),
				Trust: memory.Trust(r.Trust), Title: r.Title, Content: r.Content, Rationale: r.Rationale,
				SourceURI: deref(r.SourceUri), CreatedBy: r.CreatedBy, UpdatedAt: r.UpdatedAt,
				ApprovedBy: r.ReviewerID, ApprovedAt: r.ReviewedAt, OriginScope: r.ScopePath, Inherited: inherited[r.ScopeID], Precedence: int(r.ScopeDepth),
				Score: round4(c.score), Retrievers: slices.Clone(c.retriever), OverriddenBy: overriddenBy[r.ID],
			}
			if c.vecRank > 0 {
				sim := round4(c.vecSim)
				res.VectorSimilarity = &sim
			}
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return Response{}, err
	}
	if out == nil {
		out = []Result{}
	}
	return Response{Results: out, Meta: meta}, nil
}

func (s *Service) embedQuery(ctx context.Context, query string) ([]float32, string, string) {
	ectx, cancel := context.WithTimeout(ctx, s.cfg.QueryTimeout)
	defer cancel()
	vec, err := embeddings.EmbedQuery(ectx, s.cfg.Provider, query)
	switch {
	case err == nil:
		return vec, VectorOK, ""
	case errors.Is(err, embeddings.ErrDimensionMismatch):
		s.cfg.Logger.Error("query embedding has wrong dimensions", "err", err)
		return nil, VectorMisconfig, "embedding model is misconfigured; results are full-text only"
	case errors.Is(ectx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		s.cfg.Logger.Warn("query embedding timed out", "timeout", s.cfg.QueryTimeout)
		return nil, VectorTimeout, "query embedding timed out; results are full-text only"
	default:
		s.cfg.Logger.Warn("query embedding failed", "err", err)
		return nil, VectorUnavailable, "embedding provider unavailable; results are full-text only"
	}
}

type vectorHit struct {
	id         uuid.UUID
	similarity float64
}

// vectorCandidates returns nearest neighbours among visible, current
// revisions. The scope filter is part of the index scan's query, so
// candidates outside the chain never take a slot. With relaxed_order the
// index may return rows slightly out of order; they are re-sorted here.
func (s *Service) vectorCandidates(ctx context.Context, tx postgres.Tx, qvec []float32, scopeIDs []uuid.UUID,
	statuses, kinds []string, exclude []uuid.UUID) ([]vectorHit, error) {
	if err := embeddings.ApplyVectorSettings(ctx, tx, s.cfg.IterativeScan, s.cfg.EFSearch); err != nil {
		return nil, err
	}
	m := s.cfg.Provider.Model()
	sql := fmt.Sprintf(`SELECT e.memory_id, 1 - (%[1]s <=> %[2]s) AS similarity
FROM memory_embeddings e
JOIN memories mem ON mem.id = e.memory_id AND mem.current_revision = e.revision AND mem.scope_id = e.scope_id
WHERE %[3]s
  AND e.scope_id = ANY($2)
  AND mem.status = ANY($3)
  AND (cardinality($4::text[]) = 0 OR mem.kind = ANY($4))
  AND NOT (e.memory_id = ANY($5))
ORDER BY %[1]s <=> %[2]s
LIMIT $6`, m.VectorExpr("e"), m.ParamExpr(1), m.Predicate("e"))
	rows, err := tx.Query(ctx, sql, pgvector.NewVector(qvec), scopeIDs, statuses, kinds, exclude, s.cfg.CandidateLimit)
	if err != nil {
		return nil, fmt.Errorf("vector candidates: %w", err)
	}
	defer rows.Close()
	var out []vectorHit
	for rows.Next() {
		var h vectorHit
		if err := rows.Scan(&h.id, &h.similarity); err != nil {
			return nil, err
		}
		if h.similarity >= s.cfg.MinVectorSimilarity {
			out = append(out, h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.SortStableFunc(out, func(a, b vectorHit) int {
		switch {
		case a.similarity > b.similarity:
			return -1
		case a.similarity < b.similarity:
			return 1
		}
		return 0
	})
	return out, nil
}

// hasWebsearchSyntax reports whether the query uses websearch operators
// (quoted phrases, OR, -exclusion); such queries keep their strict meaning.
func hasWebsearchSyntax(q string) bool {
	if strings.Contains(q, `"`) {
		return true
	}
	for _, f := range strings.Fields(q) {
		if f == "OR" || (len(f) > 1 && f[0] == '-') {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func round4(f float64) float64 {
	return float64(int64(f*10000+0.5)) / 10000
}
