package embedder

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNewConfiguresTransportTimeouts pins down the hardening in New(): a
// dial/TLS-handshake/response-header timeout on the transport, but no
// overall client.Timeout — EmbedBatch legitimately pushes a large batch of
// texts through one request during ingest, and a blanket deadline would cut
// that off along with a genuinely hung connection.
func TestNewConfiguresTransportTimeouts(t *testing.T) {
	e := New()
	tr, ok := e.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", e.client.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil, want a dialer with a connect timeout")
	}
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != responseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, responseHeaderTimeout)
	}
	if e.client.Timeout != 0 {
		t.Errorf("client.Timeout = %s, want 0 (unset)", e.client.Timeout)
	}
}

// TestNewDefaultHost and TestNewHostFromEnv cover the two branches of New's
// OLLAMA_HOST handling without duplicating them into every other test.
func TestNewHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want string
	}{
		{name: "unset falls back to the default local host", env: "", want: defaultHost},
		{name: "set overrides the default", env: "http://ollama.internal:11434", want: "http://ollama.internal:11434"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OLLAMA_HOST", tc.env)
			if got := New().host; got != tc.want {
				t.Errorf("host = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEmbedAndEmbedBatch(t *testing.T) {
	for _, tc := range []struct {
		name       string
		texts      []string
		call       func(e *Embedder, texts []string) ([][]float32, error)
		wantPrefix string
	}{
		{
			name:       "Embed adds the search_query prefix",
			texts:      []string{"how do transactions work"},
			wantPrefix: queryPrefix,
			call: func(e *Embedder, texts []string) ([][]float32, error) {
				v, err := e.Embed(t.Context(), texts[0])
				return [][]float32{v}, err
			},
		},
		{
			name:       "EmbedBatch adds the search_document prefix to every text",
			texts:      []string{"chunk one", "chunk two"},
			wantPrefix: documentPrefix,
			call: func(e *Embedder, texts []string) ([][]float32, error) {
				return e.EmbedBatch(t.Context(), texts)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotInputs []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req embedRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				gotInputs = req.Input
				vecs := make([][]float32, len(req.Input))
				for i := range vecs {
					vecs[i] = []float32{float32(i), 0.5}
				}
				json.NewEncoder(w).Encode(embedResponse{Embeddings: vecs})
			}))
			defer srv.Close()

			e := &Embedder{host: srv.URL, client: srv.Client()}
			got, err := tc.call(e, tc.texts)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.texts) {
				t.Fatalf("got %d vectors, want %d", len(got), len(tc.texts))
			}
			for i, in := range gotInputs {
				if !strings.HasPrefix(in, tc.wantPrefix) {
					t.Errorf("input[%d] = %q, want prefix %q", i, in, tc.wantPrefix)
				}
				if !strings.HasSuffix(in, tc.texts[i]) {
					t.Errorf("input[%d] = %q, want suffix %q", i, in, tc.texts[i])
				}
			}
		})
	}
}

func TestEmbedManyErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{
			name: "non-200 status is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		},
		{
			name: "malformed JSON body is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not json"))
			},
		},
		{
			name: "vector count mismatch is an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(embedResponse{Embeddings: [][]float32{{1, 2}}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			e := &Embedder{host: srv.URL, client: srv.Client()}
			if _, err := e.EmbedBatch(t.Context(), []string{"a", "b"}); err == nil {
				t.Error("expected an error, got nil")
			}
		})
	}
}
