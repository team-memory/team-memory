// Package embeddings turns memory text into vectors. Providers are pluggable;
// local Ollama is the first implementation. Each provider serves exactly one
// model, and a model's dimensions are part of its identity: vectors from
// different models are never stored in, or compared within, one index.
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// EmbeddingProvider embeds a batch of texts with one model. Implementations
// must return exactly one vector per input text, in input order.
type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() Model
}

// Model describes an embedding space.
type Model struct {
	// Name identifies the model in the embedding_models registry and in the
	// provider (for Ollama, the model tag, e.g. "nomic-embed-text").
	Name       string
	Dimensions int
	// DocumentPrefix and QueryPrefix are task instructions some models expect
	// (nomic-embed-text: "search_document: " / "search_query: ").
	DocumentPrefix string
	QueryPrefix    string
}

var modelNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,99}$`)

// Validate checks the model name and dimensions against the registry's rules.
// The name is also embedded in SQL as a literal (partial-index predicates), so
// it is restricted to a safe character set.
func (m Model) Validate() error {
	if !modelNameRe.MatchString(m.Name) {
		return fmt.Errorf("embedding model name %q must match %s", m.Name, modelNameRe)
	}
	if m.Dimensions < 1 || m.Dimensions > 2000 {
		return fmt.Errorf("embedding model %s: dimensions %d out of range 1..2000", m.Name, m.Dimensions)
	}
	return nil
}

// Defaults are a model plus the similarity thresholds tuned for it.
// Thresholds are cosine similarities and are not comparable across models.
type Defaults struct {
	Model
	// MinSimilarity is the vector-candidate floor for search.
	MinSimilarity float64
	// ConflictSimilarity is the conflict-candidate threshold.
	ConflictSimilarity float64
}

// Known defaults for models evaluated in docs/eval/retrieval-report.md. The
// thresholds were chosen on that small synthetic set and are starting points,
// not calibrated values.
var knownModels = map[string]Defaults{
	"nomic-embed-text": {
		Model: Model{Name: "nomic-embed-text", Dimensions: 768,
			DocumentPrefix: "search_document: ", QueryPrefix: "search_query: "},
		MinSimilarity: 0.60, ConflictSimilarity: 0.75,
	},
	"qwen3-embedding:0.6b": {
		Model: Model{Name: "qwen3-embedding:0.6b", Dimensions: 1024,
			QueryPrefix: "Instruct: Given a question about a software team's decisions and conventions, retrieve the relevant knowledge record\nQuery: "},
		MinSimilarity: 0.50, ConflictSimilarity: 0.70,
	},
}

// KnownModel returns the built-in defaults for a model name, if any.
func KnownModel(name string) (Defaults, bool) {
	d, ok := knownModels[name]
	return d, ok
}

// ErrDimensionMismatch means the provider returned vectors whose length does
// not match the configured model. It is a configuration error, so callers
// should not retry it.
var ErrDimensionMismatch = errors.New("embedding dimension mismatch")

// ErrUnavailable wraps transport and server failures that may succeed later.
var ErrUnavailable = errors.New("embedding provider unavailable")

// checkVectors validates count and dimensions of a provider response.
func checkVectors(m Model, want int, got [][]float32) error {
	if len(got) != want {
		return fmt.Errorf("%w: provider returned %d vectors for %d inputs", ErrUnavailable, len(got), want)
	}
	for i, v := range got {
		if len(v) != m.Dimensions {
			return fmt.Errorf("%w: model %s returned %d dimensions for input %d, configured %d",
				ErrDimensionMismatch, m.Name, len(v), i, m.Dimensions)
		}
	}
	return nil
}

// EmbedDocuments embeds memory texts with the model's document prefix.
func EmbedDocuments(ctx context.Context, p EmbeddingProvider, texts []string) ([][]float32, error) {
	return p.Embed(ctx, withPrefix(p.Model().DocumentPrefix, texts))
}

// EmbedQuery embeds a search query with the model's query prefix.
func EmbedQuery(ctx context.Context, p EmbeddingProvider, query string) ([]float32, error) {
	v, err := p.Embed(ctx, withPrefix(p.Model().QueryPrefix, []string{query}))
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func withPrefix(prefix string, texts []string) []string {
	if prefix == "" {
		return texts
	}
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = prefix + t
	}
	return out
}

// DocumentText is the text embedded for a memory revision.
func DocumentText(title, content, rationale string) string {
	s := title + "\n\n" + content
	if rationale != "" {
		s += "\n\nRationale: " + rationale
	}
	return s
}
