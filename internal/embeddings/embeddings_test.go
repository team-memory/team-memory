package embeddings

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaEmbed(t *testing.T) {
	var got ollamaEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		resp := ollamaEmbedResponse{}
		for range got.Input {
			resp.Embeddings = append(resp.Embeddings, []float32{1, 0, 0})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	o, err := NewOllama(srv.URL+"/", Model{Name: "m", Dimensions: 3, QueryPrefix: "q: ", DocumentPrefix: "d: "}, nil)
	if err != nil {
		t.Fatal(err)
	}
	vs, err := EmbedDocuments(context.Background(), o, []string{"a", "b"})
	if err != nil || len(vs) != 2 {
		t.Fatalf("embed: %v %v", vs, err)
	}
	if got.Model != "m" || got.Input[0] != "d: a" || !got.Truncate {
		t.Fatalf("request: %+v", got)
	}
	if _, err := EmbedQuery(context.Background(), o, "x"); err != nil || got.Input[0] != "q: x" {
		t.Fatalf("query: %v %+v", err, got)
	}
}

func TestOllamaDimensionMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"embeddings":[[1,2,3,4]]}`))
	}))
	defer srv.Close()
	o, _ := NewOllama(srv.URL, Model{Name: "m", Dimensions: 3}, nil)
	if _, err := o.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("want dimension mismatch, got %v", err)
	}
}

func TestOllamaUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"model \"m\" not found, try pulling it first"}`))
	}))
	o, _ := NewOllama(srv.URL, Model{Name: "m", Dimensions: 3}, nil)
	if _, err := o.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable for 404, got %v", err)
	}
	srv.Close()
	if _, err := o.Embed(context.Background(), []string{"a"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable for closed server, got %v", err)
	}
}

func TestModelValidate(t *testing.T) {
	for _, m := range []Model{
		{Name: "", Dimensions: 3},
		{Name: "x'; drop table jobs;--", Dimensions: 3},
		{Name: "ok", Dimensions: 0},
		{Name: "ok", Dimensions: 2001},
	} {
		if m.Validate() == nil {
			t.Errorf("%+v: want error", m)
		}
	}
	if err := (Model{Name: "qwen3-embedding:0.6b", Dimensions: 1024}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFakeDeterministicAndSimilar(t *testing.T) {
	f := NewFake("fake", 64)
	a, _ := f.Embed(context.Background(), []string{"postgres ledger decision"})
	b, _ := f.Embed(context.Background(), []string{"postgres ledger decision"})
	c, _ := f.Embed(context.Background(), []string{"postgres ledger"})
	d, _ := f.Embed(context.Background(), []string{"kubernetes calico networking"})
	dot := func(x, y []float32) (s float32) {
		for i := range x {
			s += x[i] * y[i]
		}
		return s
	}
	if dot(a[0], b[0]) < 0.9999 {
		t.Fatal("not deterministic")
	}
	if dot(a[0], c[0]) <= dot(a[0], d[0]) {
		t.Fatalf("shared words should be closer: %v vs %v", dot(a[0], c[0]), dot(a[0], d[0]))
	}
	f.SetWrongDimensions(true)
	if _, err := f.Embed(context.Background(), []string{"x"}); !errors.Is(err, ErrDimensionMismatch) {
		t.Fatalf("want mismatch, got %v", err)
	}
}

func TestIndexNameIsDistinctAndShort(t *testing.T) {
	seen := map[string]string{}
	long := "a-very-long-model-name-from-some-registry/with-a-namespace:and-a-tag"
	for _, n := range []string{"nomic-embed-text", "nomic-embed-text:v1.5", "a.b", "a-b", "a_b", long, long + "2"} {
		name := Model{Name: n}.IndexName()
		if len(name) > 63 {
			t.Fatalf("%q: index name %q exceeds 63 bytes", n, name)
		}
		if prev, ok := seen[name]; ok {
			t.Fatalf("%q and %q share index name %q", prev, n, name)
		}
		seen[name] = n
	}
	// The migration's index for the default model uses this name.
	if got := (Model{Name: "nomic-embed-text"}).IndexName(); got != "memory_embeddings_nomic_embed_text_a4134b0e_hnsw" {
		t.Fatalf("default model index name changed: %s", got)
	}
}
