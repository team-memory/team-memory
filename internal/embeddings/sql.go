package embeddings

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/team-memory/team-memory/internal/postgres/db"
)

// SQL fragments for one model. Each model's approximate index is a partial
// expression index:
//
//	CREATE INDEX ... USING hnsw ((embedding::vector(N)) vector_cosine_ops) WHERE model = '<name>'
//
// PostgreSQL uses it only when a query repeats the same expression and a
// predicate that provably implies the index predicate, so the model name is
// inlined as a literal (a bind parameter would not match in a generic plan).
// Model.Validate restricts names to characters that are safe inside quotes.

// VectorExpr is the indexed expression, e.g. "embedding::vector(768)".
func (m Model) VectorExpr(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("%sembedding::vector(%d)", alias, m.Dimensions)
}

// ParamExpr casts a bind parameter to the model's vector type.
func (m Model) ParamExpr(n int) string {
	return fmt.Sprintf("$%d::vector(%d)", n, m.Dimensions)
}

// Predicate restricts rows to the model, matching the partial index.
func (m Model) Predicate(alias string) string {
	if alias != "" {
		alias += "."
	}
	return fmt.Sprintf("%smodel = '%s'", alias, m.Name)
}

// IndexName is the name of the model's HNSW index: a readable, sanitized
// prefix of the model name plus a hash of the full name, so distinct models
// never share a name after sanitizing or PostgreSQL's 63-byte truncation.
func (m Model) IndexName() string {
	b := []byte("memory_embeddings_")
	for i, c := range []byte(m.Name) {
		if i == 30 {
			break
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b = append(b, c)
		} else {
			b = append(b, '_')
		}
	}
	sum := sha256.Sum256([]byte(m.Name))
	return fmt.Sprintf("%s_%x_hnsw", b, sum[:4])
}

// ApplyVectorSettings sets pgvector's HNSW scan options for the current
// transaction and plans each statement for its own parameters: whether the
// HNSW index or a filtered exact scan is cheaper depends on how selective the
// request's scope list is, which a cached generic plan cannot know. See
// docs/decisions/vector-index-strategy.md.
func ApplyVectorSettings(ctx context.Context, tx pgx.Tx, iterativeScan string, efSearch int) error {
	_, err := tx.Exec(ctx, `SELECT set_config('hnsw.iterative_scan', $1, true), set_config('hnsw.ef_search', $2, true),
		set_config('plan_cache_mode', 'force_custom_plan', true)`, iterativeScan, strconv.Itoa(efSearch))
	if err != nil {
		return fmt.Errorf("vector settings: %w", err)
	}
	return nil
}

// CreateIndexSQL builds the model's HNSW index. Operators run it through
// `team-memory embeddings add-model`; it needs table-owner privileges.
func (m Model) CreateIndexSQL(concurrently bool) string {
	c := ""
	if concurrently {
		c = "CONCURRENTLY "
	}
	return fmt.Sprintf("CREATE INDEX %sIF NOT EXISTS %s ON memory_embeddings USING hnsw ((%s) vector_cosine_ops) WHERE %s",
		c, m.IndexName(), m.VectorExpr(""), m.Predicate(""))
}

// AddModel registers a model and builds its HNSW index. It is an operator
// path (DDL needs table-owner privileges). An existing registration with the
// same dimensions is accepted, so the call is safe to repeat; different
// dimensions are rejected.
func AddModel(ctx context.Context, pool *pgxpool.Pool, m Model, concurrently bool) error {
	if err := m.Validate(); err != nil {
		return err
	}
	q := db.New(pool)
	existing, err := q.GetEmbeddingModel(ctx, m.Name)
	switch {
	case err == nil && int(existing.Dimensions) != m.Dimensions:
		return fmt.Errorf("%w: model %s is already registered with %d dimensions", ErrDimensionMismatch, m.Name, existing.Dimensions)
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := q.InsertEmbeddingModel(ctx, db.InsertEmbeddingModelParams{
			Name: m.Name, Dimensions: int32(m.Dimensions), Enabled: true,
		}); err != nil {
			return err
		}
	case err != nil:
		return err
	}
	if _, err := pool.Exec(ctx, m.CreateIndexSQL(concurrently)); err != nil {
		return err
	}
	// IF NOT EXISTS also skips an INVALID index left by a failed concurrent
	// build, so confirm the index is usable.
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT i.indisvalid FROM pg_index i
		JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1), false)`, m.IndexName()).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("index %s exists but is not valid (an earlier build failed); DROP INDEX it and run add-model again", m.IndexName())
	}
	return nil
}
