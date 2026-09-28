package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Dim is the embedding dimension produced by nomic-embed-text.
const Dim = 768

const defaultHost = "http://localhost:11434"
const model = "nomic-embed-text"

// Transport timeouts. No overall http.Client.Timeout: EmbedBatch (the ingest
// path) can legitimately push a large batch of texts through one request, and
// a blanket deadline on the whole request/response cycle would cut that off
// along with everything else. These bound the parts that should never take
// long even for a big batch — connecting and getting the first response byte
// — while leaving the body-read time uncapped. Per-call cancellation is the
// caller's job via ctx (see answer.go's embedTimeout for the query path).
const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 30 * time.Second
)

// Embedder calls the Ollama /api/embed endpoint.
type Embedder struct {
	host   string
	client *http.Client
}

// New creates an Embedder. Reads OLLAMA_HOST from the environment (default: http://localhost:11434).
func New() *Embedder {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = defaultHost
	}
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: dialTimeout}).DialContext,
		TLSHandshakeTimeout:   tlsHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
	}
	return &Embedder{host: host, client: &http.Client{Transport: transport}}
}

// nomic-embed-text is trained with task prefixes: queries and documents live
// in different regions of the embedding space. Using them improves retrieval.
// See https://huggingface.co/nomic-ai/nomic-embed-text-v1.5#usage
const (
	queryPrefix    = "search_query: "
	documentPrefix = "search_document: "
)

// Embed returns the embedding vector for a search query (user question).
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := e.embedMany(ctx, []string{queryPrefix + text})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedBatch returns embeddings for document chunks (ingest side).
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	prefixed := make([]string, len(texts))
	for i, t := range texts {
		prefixed[i] = documentPrefix + t
	}
	return e.embedMany(ctx, prefixed)
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *Embedder) embedMany(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: model, Input: texts})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.host+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: HTTP %d", resp.StatusCode)
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: expected %d vectors, got %d", len(texts), len(result.Embeddings))
	}
	return result.Embeddings, nil
}
