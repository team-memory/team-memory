package embeddings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Ollama calls the Ollama /api/embed endpoint.
type Ollama struct {
	baseURL string
	model   Model
	client  *http.Client
}

// NewOllama returns a provider for one model served by Ollama at baseURL.
func NewOllama(baseURL string, m Model, client *http.Client) (*Ollama, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("ollama url %q must be an absolute http(s) URL", baseURL)
	}
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &Ollama{baseURL: strings.TrimRight(baseURL, "/"), model: m, client: client}, nil
}

func (o *Ollama) Model() Model { return o.model }

type ollamaEmbedRequest struct {
	Model    string   `json:"model"`
	Input    []string `json:"input"`
	Truncate bool     `json:"truncate"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed sends one batch. Inputs longer than the model's context are truncated
// by Ollama (truncate=true), so very long memories are embedded from their
// beginning only.
func (o *Ollama) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(ollamaEmbedRequest{Model: o.model.Name, Input: texts, Truncate: true})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %v", ErrUnavailable, err)
	}
	var out ollamaEmbedResponse
	if resp.StatusCode != http.StatusOK {
		_ = json.Unmarshal(raw, &out)
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw[:min(len(raw), 200)]))
		}
		return nil, fmt.Errorf("%w: ollama returned %d: %s", ErrUnavailable, resp.StatusCode, msg)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrUnavailable, err)
	}
	if err := checkVectors(o.model, len(texts), out.Embeddings); err != nil {
		return nil, err
	}
	return out.Embeddings, nil
}
