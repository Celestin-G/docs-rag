package server

// Round 2 review tests: exercise the shutdown machinery against the REAL
// go-sdk streamable HTTP handler wired by New(), not a fake that merely
// blocks on r.Context().

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

const reviewProtoVersion = "2025-06-18"

// newReviewServer builds the real handler via New() (MCP_TOKEN set) and
// serves it behind an http.Server whose BaseContext mirrors cmd/serve.go.
func newReviewServer(t *testing.T, token string) (h *Server, httpSrv *http.Server, base string, cancelBase context.CancelFunc) {
	t.Helper()
	t.Setenv("MCP_TOKEN", token)
	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s.logger = &countingLogger{}
	baseCtx, cancelBase := context.WithCancel(context.Background())
	t.Cleanup(cancelBase)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv = &http.Server{Handler: h, BaseContext: func(net.Listener) context.Context { return baseCtx }}
	httpSrv.RegisterOnShutdown(h.CancelStreams)
	go httpSrv.Serve(ln)
	t.Cleanup(func() { httpSrv.Close() })
	return h, httpSrv, "http://" + ln.Addr().String(), cancelBase
}

func mcpReq(t *testing.T, method, url, token, session, body string) *http.Request {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r, err := http.NewRequest(method, url+"/api/mcp", rd)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Accept", "application/json, text/event-stream")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		r.Header.Set("Mcp-Session-Id", session)
		r.Header.Set("Mcp-Protocol-Version", reviewProtoVersion)
	}
	return r
}

// mcpInit performs initialize + notifications/initialized and returns the session id.
func mcpInit(t *testing.T, base, token string) string {
	t.Helper()
	resp, err := http.DefaultClient.Do(mcpReq(t, http.MethodPost, base, token, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+reviewProtoVersion+`","capabilities":{},"clientInfo":{"name":"review","version":"1"}}}`))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no Mcp-Session-Id returned")
	}
	resp, err = http.DefaultClient.Do(mcpReq(t, http.MethodPost, base, token, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if err != nil {
		t.Fatalf("initialized notification: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return sid
}

// A real go-sdk GET stream, opened through New()'s mux (auth + rate limit +
// withStreamCancel), must be released by Shutdown via RegisterOnShutdown.
func TestReviewRealMCPGetStreamReleasedOnShutdown(t *testing.T) {
	_, httpSrv, base, _ := newReviewServer(t, "tok")
	sid := mcpInit(t, base, "tok")

	got := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(mcpReq(t, http.MethodGet, base, "tok", sid, ""))
		if err != nil {
			got <- -1
			return
		}
		got <- resp.StatusCode
		io.Copy(io.Discard, resp.Body) // blocks until the server ends the stream
		resp.Body.Close()
		got <- 0
	}()
	if code := <-got; code != http.StatusOK {
		t.Fatalf("GET stream status = %d, want 200", code)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := httpSrv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with an open real MCP GET stream: %v (after %s)", err, time.Since(start))
	}
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("GET stream body never ended after Shutdown")
	}
}

// Auth must still apply to GET /api/mcp after withStreamCancel was wrapped
// around the whole MCP chain in New().
func TestReviewMCPGetThroughNewRequiresAuth(t *testing.T) {
	t.Setenv("MCP_TOKEN", "tok")
	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, auth := range []string{"", "Bearer wrong"} {
		r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		r.RemoteAddr = "203.0.113.90:1"
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("GET /api/mcp auth=%q: status = %d, want 401", auth, w.Code)
		}
	}
}

// Rate limit must still apply to GET /api/mcp (and count failed-auth GETs).
func TestReviewMCPGetThroughNewIsRateLimited(t *testing.T) {
	t.Setenv("MCP_TOKEN", "tok")
	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var last int
	for i := 0; i <= mcpRateBurst; i++ {
		r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		r.RemoteAddr = "203.0.113.91:1"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		last = w.Code
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("GET /api/mcp request %d: status = %d, want 429", mcpRateBurst+1, last)
	}
}

// After CancelStreams, POST /api/mcp (initialize, a short call) must still
// succeed: streamCtx must not leak onto the POST route.
func TestReviewMCPPostUnaffectedByCancelStreams(t *testing.T) {
	h, _, base, _ := newReviewServer(t, "tok")
	h.CancelStreams()
	_ = mcpInit(t, base, "tok") // fatals on non-200
}

// A GET that arrives after CancelStreams (e.g. on a keep-alive conn racing
// Shutdown) must be released immediately, not hang.
func TestReviewStreamCancelAlreadyCancelled(t *testing.T) {
	s := &srv{}
	s.streamCtx, s.cancelStreams = context.WithCancel(context.Background())
	s.cancelStreams()
	done := make(chan struct{})
	go func() {
		s.withStreamCancel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/mcp", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("GET after CancelStreams was not released")
	}
}

// logRequest racing Wait: every call must be either recorded or rejected,
// never panic, never race (run with -race), never run after Wait returns.
func TestReviewLogRequestConcurrentWithWait(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		logger := &fakeLogger{}
		s := &srv{logger: logger}
		h := &Server{s: s}
		var wg sync.WaitGroup
		var afterWait atomic.Bool
		var lateWrites atomic.Int32
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.logRequest("q", 1, 0, "ok")
			}()
		}
		h.Wait()
		afterWait.Store(true)
		before := logger.calls.Load()
		wg.Wait()
		time.Sleep(5 * time.Millisecond)
		if got := logger.calls.Load(); got != before {
			lateWrites.Add(got - before)
		}
		if lateWrites.Load() != 0 {
			t.Fatalf("iter %d: %d request-log writes ran after Wait returned", iter, lateWrites.Load())
		}
	}
}

// The forced-cancel path (cancel BaseContext + Close) is documented to
// unblock "every request's context", MCP tool calls included. go-sdk v1.7.0
// runs tool handlers on a jsonrpc2 connection whose ctx is notDone{initialize
// request ctx}, so this checks whether askDocs' upstream LLM call really gets
// cancelled when cmd/serve.go's DeadlineExceeded branch runs.
func TestReviewForcedCancelReachesMCPToolCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	upstreamCancelled := make(chan struct{}, 1)
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			upstreamCancelled <- struct{}{}
		case <-time.After(10 * time.Second):
		}
	}))
	defer llmSrv.Close()
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer embSrv.Close()
	t.Setenv("LLM_BASE_URL", llmSrv.URL+"/v1")
	t.Setenv("OLLAMA_HOST", embSrv.URL)

	h, httpSrv, base, cancelBase := newReviewServer(t, "tok")
	h.s.llm = llm.New()
	h.s.emb = embedder.New()
	h.s.translateTimeout = 8 * time.Second
	h.s.totalAnswerBudget = 9 * time.Second

	sid := mcpInit(t, base, "tok")
	go func() {
		resp, err := http.DefaultClient.Do(mcpReq(t, http.MethodPost, base, "tok", sid,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_formance_docs","arguments":{"question":"what is a ledger"}}}`))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool call never reached the LLM translate stage")
	}

	// Exactly cmd/serve.go's DeadlineExceeded branch.
	cancelBase()
	h.ForceCancel()
	httpSrv.Close()

	select {
	case <-upstreamCancelled:
	case <-time.After(2 * time.Second):
		t.Error("after cancelBase()+Close(), the MCP tool call's upstream LLM request was NOT cancelled: " +
			"askDocs' ctx is not derived from BaseContext (go-sdk jsonrpc2 notDone ctx), so the forced-cancel " +
			"path does not stop MCP tool calls as README/serve.go claim")
	}
	// askDocs is tracked by handlersWG, and once cancelled it must unwind
	// promptly, so WaitHandlers reports "all done" only once it really is.
	if !h.WaitHandlers(2 * time.Second) {
		t.Error("WaitHandlers did not see askDocs return after the forced cancel")
	}
}
