package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestNewConfiguresTransportTimeouts pins down the hardening in New(): a
// dial/TLS-handshake/response-header timeout on the transport, but
// deliberately no overall client.Timeout — Ask's response is a long-lived
// SSE stream, and a blanket deadline would sever it mid-generation.
func TestNewConfiguresTransportTimeouts(t *testing.T) {
	c := New()
	tr, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", c.client.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext is nil, want a dialer with a connect timeout")
	}
	if tr.TLSHandshakeTimeout != tlsHandshakeTimeout {
		t.Errorf("TLSHandshakeTimeout = %s, want %s", tr.TLSHandshakeTimeout, tlsHandshakeTimeout)
	}
	if tr.ResponseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Errorf("ResponseHeaderTimeout = %s, want %s", tr.ResponseHeaderTimeout, defaultResponseHeaderTimeout)
	}
	if c.client.Timeout != 0 {
		t.Errorf("client.Timeout = %s, want 0 (unset)", c.client.Timeout)
	}
}

// TestResponseHeaderTimeoutEnv covers LLM_RESPONSE_HEADER_TIMEOUT's parsing
// contract: unset, malformed, or non-positive all fall back to
// defaultResponseHeaderTimeout instead of panicking or producing a zero
// (immediately-expired) transport timeout, which would fail every outbound
// call before it even started. Mirrors internal/server's TestEnvDuration /
// TestEnvDurationRejectsNonPositive, since this is a deliberate duplicate of
// that exact contract (see responseHeaderTimeout's doc comment for why it
// isn't shared code).
func TestResponseHeaderTimeoutEnv(t *testing.T) {
	const key = "LLM_RESPONSE_HEADER_TIMEOUT"

	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses the default", env: "", want: defaultResponseHeaderTimeout},
		{name: "valid duration overrides the default", env: "90s", want: 90 * time.Second},
		{name: "malformed value falls back to the default", env: "not-a-duration", want: defaultResponseHeaderTimeout},
		{name: "unitless number falls back to the default", env: "60", want: defaultResponseHeaderTimeout},
		{name: "zero falls back to the default", env: "0s", want: defaultResponseHeaderTimeout},
		{name: "negative duration falls back to the default", env: "-1s", want: defaultResponseHeaderTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.env)
			if got := responseHeaderTimeout(); got != tc.want {
				t.Errorf("responseHeaderTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestNewReadsResponseHeaderTimeoutFromEnv pins down that New() actually
// wires LLM_RESPONSE_HEADER_TIMEOUT into the transport it builds, exercised
// through the constructor that actually calls responseHeaderTimeout() rather
// than the helper in isolation.
func TestNewReadsResponseHeaderTimeoutFromEnv(t *testing.T) {
	t.Setenv("LLM_RESPONSE_HEADER_TIMEOUT", "12s")
	c := New()
	tr, ok := c.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("client.Transport = %T, want *http.Transport", c.client.Transport)
	}
	if tr.ResponseHeaderTimeout != 12*time.Second {
		t.Errorf("ResponseHeaderTimeout = %s, want 12s", tr.ResponseHeaderTimeout)
	}
}

// TestTranslate covers Translate's graceful-degradation contract: on any
// failure — slow backend past the caller's own timeout, non-JSON body, or an
// empty completion — it returns the original text alongside the error
// instead of leaving the caller with nothing to fall back to.
func TestTranslate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		handler    http.HandlerFunc
		ctxTimeout time.Duration // 0 means no per-call timeout
		wantText   string
		wantErr    bool
	}{
		{
			name: "successful translation is returned",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
					Message Message `json:"message"`
				}{{Message: Message{Role: "assistant", Content: "hello"}}}})
			},
			wantText: "hello",
		},
		{
			name: "empty completion falls back to the original text",
			handler: func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(chatResponse{})
			},
			wantText: "bonjour",
		},
		{
			name: "malformed JSON body falls back to the original text, with an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("not json"))
			},
			wantText: "bonjour",
			wantErr:  true,
		},
		{
			name: "a backend slower than the caller's timeout falls back to the original text, with an error",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(200 * time.Millisecond)
				json.NewEncoder(w).Encode(chatResponse{Choices: []struct {
					Message Message `json:"message"`
				}{{Message: Message{Role: "assistant", Content: "hello"}}}})
			},
			ctxTimeout: 20 * time.Millisecond,
			wantText:   "bonjour",
			wantErr:    true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := &Client{baseURL: srv.URL, model: "test-model", client: srv.Client()}

			ctx := t.Context()
			if tc.ctxTimeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.ctxTimeout)
				defer cancel()
			}

			got, err := c.Translate(ctx, "bonjour")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.wantText {
				t.Errorf("Translate() = %q, want %q", got, tc.wantText)
			}
		})
	}
}
