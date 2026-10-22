package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"

	"github.com/team-memory/team-memory/internal/embeddings"
	"github.com/team-memory/team-memory/internal/postgres"
)

// embedSyntheticCmd gives every current revision a vector without running
// the model per document: each vocabulary word is embedded once by the real
// model, and a document's vector is the normalized sum of its words'
// vectors. Queries are still embedded by the model at run time, so query
// and document vectors share one space. Used because CPU-only Ollama in a
// Docker Desktop VM embeds about 0.6 documents/s (see the load-test report).
func embedSyntheticCmd(ctx context.Context, args []string) error {
	var ollama, model string
	flags("embed-synthetic", args, func(fs *flag.FlagSet) {
		fs.StringVar(&ollama, "ollama", "http://127.0.0.1:11434", "Ollama URL")
		fs.StringVar(&model, "model", "nomic-embed-text", "registered model")
	})
	known, ok := embeddings.KnownModel(model)
	if !ok {
		return fmt.Errorf("unknown model %s", model)
	}
	p, err := embeddings.NewOllama(ollama, known.Model, &http.Client{Timeout: 10 * time.Minute})
	if err != nil {
		return err
	}
	pool, err := postgres.Open(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	start := time.Now()
	texts := make([]string, len(vocabulary))
	for i, w := range vocabulary {
		texts[i] = known.DocumentPrefix + w
	}
	var vecs [][]float32
	for i := 0; i < len(texts); i += 8 {
		v, err := p.Embed(ctx, texts[i:min(i+8, len(texts))])
		if err != nil {
			return err
		}
		vecs = append(vecs, v...)
	}
	word := map[string][]float32{}
	for i, w := range vocabulary {
		word[w] = vecs[i]
	}
	fmt.Printf("embedded %d words in %s\n", len(vocabulary), time.Since(start).Round(time.Millisecond))
	rows, err := pool.Query(ctx, `SELECT r.memory_id, r.revision, r.org_id, r.scope_id, r.title || ' ' || r.content
FROM memories m JOIN memory_revisions r ON r.memory_id = m.id AND r.revision = m.current_revision
WHERE NOT EXISTS (SELECT 1 FROM memory_embeddings e WHERE e.memory_id = r.memory_id AND e.revision = r.revision AND e.model = $1)`, model)
	if err != nil {
		return err
	}
	type doc struct {
		id, org, scope any
		rev            int32
		vec            pgvector.Vector
	}
	var docs []doc
	for rows.Next() {
		var d doc
		var text string
		if err := rows.Scan(&d.id, &d.rev, &d.org, &d.scope, &text); err != nil {
			return err
		}
		sum := make([]float32, known.Dimensions)
		for _, w := range strings.Fields(strings.ToLower(strings.Trim(text, "."))) {
			if v, ok := word[strings.Trim(w, ".,")]; ok {
				for i := range sum {
					sum[i] += v[i]
				}
			}
		}
		var n float64
		for _, x := range sum {
			n += float64(x) * float64(x)
		}
		n = math.Sqrt(n)
		for i := range sum {
			sum[i] = float32(float64(sum[i]) / max(n, 1e-9))
		}
		d.vec = pgvector.NewVector(sum)
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range docs {
		if _, err := pool.Exec(ctx, `INSERT INTO memory_embeddings (memory_id, revision, org_id, scope_id, model, dimensions, embedding)
VALUES ($1, $2, $3, $4, $5, $6, $7)`, d.id, d.rev, d.org, d.scope, model, known.Dimensions, d.vec); err != nil {
			return err
		}
	}
	tag, err := pool.Exec(ctx, `UPDATE jobs SET status = 'succeeded', last_error = 'synthetic embedding (load test)', updated_at = now()
WHERE kind = 'embed' AND status IN ('pending', 'running')`)
	if err != nil {
		return err
	}
	_, _ = pool.Exec(ctx, "ANALYZE memory_embeddings")
	fmt.Printf("inserted %d synthetic embeddings, closed %d embed jobs, in %s\n", len(docs), tag.RowsAffected(), time.Since(start).Round(time.Second))
	return nil
}
