package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// Transport timeouts. No overall http.Client.Timeout: per-call cancellation is
// the caller's job via ctx (see answer.go's embedTimeout for the query path).
// Ollama's /api/embed is not streamed — it writes the response headers only
// once every input is embedded — so responseHeaderTimeout bounds the whole
// compute of one request. On the in-cluster CPU-only Ollama a full-window
// input (2048 tokens) alone takes ~24s, so 30s was too tight: two of them in
// one request failed every nightly ingest on membership.oas.json. 2 min leaves
// a wide margin over the largest request EmbedBatch sends. The query path is
// not affected: answer.go's embedTimeout cuts it off much sooner.
const (
	dialTimeout           = 5 * time.Second
	tlsHandshakeTimeout   = 5 * time.Second
	responseHeaderTimeout = 2 * time.Minute
)

// A request carries at most embedBatchSize texts and maxBatchBytes of text,
// whichever comes first; a text larger than maxBatchBytes goes alone. Normal
// chunks cost ~0.7s each, so 16 stays around 10s. Generated OpenAPI chunks
// can run to 200KB — the model truncates them to its 2048-token window, but
// each still costs ~24s — so the byte cap keeps them out of each other's
// request. Smaller requests also mean a retry redoes seconds of work.
const (
	embedBatchSize = 16
	maxBatchBytes  = 16_000
)

// defaultRetryDelays is the backoff between EmbedBatch attempts, ~4 min in
// total: enough to ride out Ollama being rescheduled onto another node (seen
// at ~2 min), short enough that a dead Ollama still fails the run promptly.
var defaultRetryDelays = []time.Duration{
	2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second,
	40 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second,
}

// Embedder calls the Ollama /api/embed endpoint.
type Embedder struct {
	host   string
	client *http.Client
	// retryDelays is the backoff EmbedBatch waits between attempts; nil means
	// a single attempt. Embed (the query path) never retries: a user is
	// waiting, and answer.go's embedTimeout bounds it anyway.
	retryDelays []time.Duration
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
	return &Embedder{host: host, client: &http.Client{Transport: transport}, retryDelays: defaultRetryDelays}
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

// EmbedBatch returns embeddings for document chunks (ingest side). Texts go
// out in small requests (see embedBatchSize), and each request is retried with
// backoff on a transient failure, so an Ollama restart mid-ingest stalls the
// run instead of failing it.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for _, batch := range batches(texts) {
		prefixed := make([]string, len(batch))
		for i, t := range batch {
			prefixed[i] = documentPrefix + t
		}
		vecs, err := e.embedWithRetry(ctx, prefixed)
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// batches splits texts into consecutive requests bounded by embedBatchSize
// and maxBatchBytes, preserving order.
func batches(texts []string) [][]string {
	var out [][]string
	start, size := 0, 0
	for i, t := range texts {
		if i > start && (i-start == embedBatchSize || size+len(t) > maxBatchBytes) {
			out = append(out, texts[start:i])
			start, size = i, 0
		}
		size += len(t)
	}
	if start < len(texts) {
		out = append(out, texts[start:])
	}
	return out
}

func (e *Embedder) embedWithRetry(ctx context.Context, texts []string) ([][]float32, error) {
	for attempt := 0; ; attempt++ {
		vecs, err := e.embedMany(ctx, texts)
		// Stop when the caller gave up (ctx), not merely because err looks like
		// a deadline: a transport timeout also matches context.DeadlineExceeded.
		if err == nil || ctx.Err() != nil || !retryable(err) || attempt >= len(e.retryDelays) {
			return vecs, err
		}
		delay := e.retryDelays[attempt]
		fmt.Fprintf(os.Stderr, "embed attempt %d failed, retrying in %s: %v\n", attempt+1, delay, err)
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(delay):
		}
	}
}

// statusError is a non-200 answer from Ollama.
type statusError struct{ code int }

func (e *statusError) Error() string { return fmt.Sprintf("ollama embed: HTTP %d", e.code) }

// retryable reports whether err is worth another attempt: transport failures
// (refused, reset, EOF, timeout — Ollama restarting, rescheduled or
// overloaded) and 5xx/429. Other 4xx mean the request itself is wrong and
// would fail again. The caller's own cancellation is checked separately.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests
	}
	return !errors.Is(err, errCountMismatch)
}

var errCountMismatch = errors.New("ollama embed: vector count mismatch")

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
		return nil, &statusError{code: resp.StatusCode}
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(result.Embeddings) != len(texts) {
		return nil, fmt.Errorf("%w: expected %d, got %d", errCountMismatch, len(texts), len(result.Embeddings))
	}
	return result.Embeddings, nil
}
