package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
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

// A GET that arrives after CancelStreams already fired (e.g. racing
// Shutdown on a keep-alive conn) must be released immediately, not hang.
func TestStreamCancelAlreadyCancelled(t *testing.T) {
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

// newShutdownTestServer builds the real handler via New() (MCP_TOKEN set) and
// serves it behind an http.Server whose BaseContext mirrors cmd/serve.go. When
// fu is non-nil, the embedder/LLM are wired to newTestSrv's fake upstreams
// (embed answers HTTP 500) instead of the zero-value nil clients, for tests
// that need to drive a real tool call through the pipeline.
func newShutdownTestServer(t *testing.T, token string, fu *fakeUpstreams) (h *Server, httpSrv *http.Server, base string, cancelBase context.CancelFunc) {
	t.Helper()
	t.Setenv("MCP_TOKEN", token)
	var emb *embedder.Embedder
	var lm *llm.Client
	if fu != nil {
		fake := newTestSrv(t, false, false, fu)
		emb, lm = fake.emb, fake.llm
	}
	h, err := New(nil, emb, lm)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.s.logger = &fakeLogger{}
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

const shutdownTestProtoVersion = "2025-06-18"

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
		r.Header.Set("Mcp-Protocol-Version", shutdownTestProtoVersion)
	}
	return r
}

// do sends req and drains + closes the response body, so callers that only
// care about the status/headers don't each repeat that boilerplate.
func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

// doBody sends req and returns the response with its body read into memory,
// for callers that (unlike do) need to inspect the payload.
func doBody(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("%s %s: reading body: %v", req.Method, req.URL, err)
	}
	return resp, body
}

// jsonRPCPayload extracts the JSON-RPC message from a streamable-HTTP
// response body, which arrives either as a bare JSON object or as a single
// "data: <json>" SSE event depending on the transport's response mode.
func jsonRPCPayload(body []byte) []byte {
	if i := bytes.Index(body, []byte("data: ")); i >= 0 {
		payload := body[i+len("data: "):]
		if j := bytes.IndexByte(payload, '\n'); j >= 0 {
			payload = payload[:j]
		}
		return payload
	}
	return body
}

// mcpInit performs initialize + notifications/initialized and returns the session id.
func mcpInit(t *testing.T, base, token string) string {
	t.Helper()
	resp := do(t, mcpReq(t, http.MethodPost, base, token, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+shutdownTestProtoVersion+`","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	sid := resp.Header.Get("Mcp-Session-Id")
	if sid == "" {
		t.Fatal("no Mcp-Session-Id returned")
	}
	do(t, mcpReq(t, http.MethodPost, base, token, sid, `{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	return sid
}

// A real go-sdk GET stream must be released by Shutdown via
// RegisterOnShutdown, and CancelStreams firing must never affect POST.
func TestRealMCPGetStreamReleasedOnShutdown(t *testing.T) {
	fu := &fakeUpstreams{}
	h, httpSrv, base, _ := newShutdownTestServer(t, "tok", fu)
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

	// httpSrv's listener is gone once Shutdown returns, so re-serve the same
	// handler on a fresh one to prove POST tool calls still work — and, per
	// withStreamCancel's contract (it is wired only to the GET route), are
	// unaffected by CancelStreams having fired. A replayed initialize alone
	// cannot tell that apart, since the go-sdk's initialize handling ignores
	// context cancellation either way; a real tools/call through the pipeline
	// can, because the embed stage actually reads its context.
	h.CancelStreams()
	// Reset the MCP rate limiter: it's shared with the first server (same h),
	// and the handshake plus GET stream above already spent most of its
	// small test-friendly burst — a limit that exists to bound abuse, not to
	// constrain how many requests this test itself may issue.
	h.s.mcpLimiter = newRateLimiter(mcpRatePerMinute, mcpRateBurst)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv2 := &http.Server{Handler: h}
	go httpSrv2.Serve(ln2)
	t.Cleanup(func() { httpSrv2.Close() })
	base2 := "http://" + ln2.Addr().String()
	sid2 := mcpInit(t, base2, "tok") // fatals on non-200

	resp, body := doBody(t, mcpReq(t, http.MethodPost, base2, "tok", sid2,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ask_formance_docs","arguments":{"question":"What is a ledger?"}}}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/call status = %d, body = %s", resp.StatusCode, body)
	}

	var rpcResp struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(jsonRPCPayload(body), &rpcResp); err != nil {
		t.Fatalf("unmarshal tools/call response %s: %v", body, err)
	}
	if rpcResp.Error != nil {
		t.Fatalf("tools/call returned a protocol error: %+v (body = %s)", rpcResp.Error, body)
	}
	if rpcResp.Result == nil || !rpcResp.Result.IsError || len(rpcResp.Result.Content) == 0 {
		t.Fatalf("tools/call result = %+v, want a tool error (the fake embed endpoint answers HTTP 500)", rpcResp.Result)
	}
	toolErr := rpcResp.Result.Content[0].Text
	if !strings.Contains(toolErr, "500") {
		t.Errorf("tool error = %q, want it to mention the embed stage's HTTP 500", toolErr)
	}
	if strings.Contains(toolErr, "context canceled") || strings.Contains(toolErr, "shutting down") {
		t.Errorf("tool error = %q: POST must not be affected by CancelStreams (see withStreamCancel)", toolErr)
	}
}

// askDocs runs on a context that never cancels on its own (the go-sdk's
// notDone tool context); ForceCancel alone must stop it and be observed by
// WaitHandlers.
func TestAskDocsTrackedAndStoppedByForceCancel(t *testing.T) {
	fu := &fakeUpstreams{entered: make(chan struct{}, 1)}
	fake := newTestSrv(t, true, false, fu) // translate stage hangs
	t.Setenv("MCP_TOKEN", "tok")

	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.s.logger = &fakeLogger{}
	h.s.llm, h.s.emb = fake.llm, fake.emb
	h.s.translateTimeout, h.s.totalAnswerBudget = fake.translateTimeout, fake.totalAnswerBudget

	done := make(chan error, 1)
	go func() {
		_, _, err := h.s.askDocs(context.Background(), nil, askDocsInput{Question: "what is a ledger"})
		done <- err
	}()
	select {
	case <-fu.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("askDocs never reached the LLM")
	}

	if h.WaitHandlers(100 * time.Millisecond) {
		t.Fatal("WaitHandlers returned true while askDocs was still running")
	}
	h.ForceCancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("askDocs returned nil error after ForceCancel, want a cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("askDocs did not return after ForceCancel")
	}
	if !h.WaitHandlers(2 * time.Second) {
		t.Error("WaitHandlers still false after askDocs returned")
	}
	// ForceCancel and CancelStreams must be idempotent: runServe calls each
	// both inline and via defer.
	h.ForceCancel()
	h.CancelStreams()
	h.CancelStreams()
}

// Once WaitHandlers has started draining, new work is refused: tool calls
// get a tool error, HTTP requests a 503, neither touches handlersWG or logs.
func TestWorkRefusedAfterWaitHandlers(t *testing.T) {
	logger := &fakeLogger{}
	h := &Server{mux: http.NewServeMux(), s: &srv{logger: logger}}
	if !h.WaitHandlers(time.Second) {
		t.Fatal("WaitHandlers returned false with nothing in flight")
	}

	t.Run("mcp tool call", func(t *testing.T) {
		_, _, err := h.s.askDocs(context.Background(), nil, askDocsInput{Question: "q"})
		if !errors.Is(err, errShuttingDown) {
			t.Errorf("askDocs err = %v, want %v", err, errShuttingDown)
		}
		if got := logger.calls.Load(); got != 0 {
			t.Errorf("refused tool call logged %d request(s), want 0", got)
		}
		if !h.WaitHandlers(100 * time.Millisecond) {
			t.Error("refused tool call left handlersWG non-zero")
		}
	})
	t.Run("http request", func(t *testing.T) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503", w.Code)
		}
		if got := w.Header().Get("Connection"); got != "close" {
			t.Errorf("Connection = %q, want close", got)
		}
	})
}

// A request that is already in flight when Shutdown begins must complete
// normally (200), not get 503: WaitHandlers only runs after Shutdown returns.
func TestInFlightRequestDuringGracefulShutdownIsNot503(t *testing.T) {
	h, httpSrv, base, _ := newShutdownTestServer(t, "tok", nil)
	unblock := make(chan struct{})
	entered := make(chan struct{})
	var once sync.Once
	h.s.ping = pingerFunc(func(ctx context.Context) error {
		once.Do(func() { close(entered) })
		select {
		case <-unblock:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	status := make(chan int, 1)
	go func() {
		resp, err := http.Get(base + "/readyz")
		if err != nil {
			status <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		status <- resp.StatusCode
	}()
	<-entered

	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownDone <- httpSrv.Shutdown(ctx)
	}()
	time.Sleep(100 * time.Millisecond) // let Shutdown start draining
	close(unblock)

	if got := <-status; got != http.StatusOK {
		t.Errorf("in-flight request during graceful Shutdown: status = %d, want 200", got)
	}
	if err := <-shutdownDone; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if !h.WaitHandlers(time.Second) {
		t.Error("WaitHandlers false after graceful Shutdown completed")
	}
}

// Concurrent requests racing WaitHandlers: each is admitted (and counted
// until it returns) or refused with 503 before reaching the mux. Run under -race.
func TestEnterHandlerRacesWaitHandlers(t *testing.T) {
	for iter := 0; iter < 3; iter++ {
		var running, served atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			running.Add(1)
			time.Sleep(2 * time.Millisecond)
			served.Add(1)
			running.Add(-1)
		})
		h := &Server{mux: mux, s: &srv{logger: &fakeLogger{}}}
		var wg sync.WaitGroup
		var refused atomic.Int32
		const n = 8
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
				if w.Code == http.StatusServiceUnavailable {
					refused.Add(1)
				}
			}()
		}
		if !h.WaitHandlers(2 * time.Second) {
			t.Fatalf("iter %d: WaitHandlers timed out", iter)
		}
		if r := running.Load(); r != 0 {
			t.Fatalf("iter %d: WaitHandlers returned with %d handler(s) still running", iter, r)
		}
		wg.Wait()
		if got := served.Load() + refused.Load(); got != n {
			t.Fatalf("iter %d: served %d + refused %d != %d", iter, served.Load(), refused.Load(), n)
		}
	}
}

// WaitHandlers returns false (bounded by timeout) while a handler runs, and
// true once it returns — including on a second call after the first timed out.
func TestWaitHandlersBoundedByTimeout(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	})
	h := &Server{mux: mux, s: &srv{}}

	go func() {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	if h.WaitHandlers(100 * time.Millisecond) {
		t.Fatal("WaitHandlers returned true while a handler is still running")
	}

	close(release)

	if !h.WaitHandlers(2 * time.Second) {
		t.Fatal("WaitHandlers did not return true after the handler finished")
	}
}

// A logRequest call after (*Server).Wait started draining must not panic
// (WaitGroup misuse) or vanish silently — it is rejected, and logged as such.
func TestLogRequestAfterClosingDoesNotPanicAndLogsRejection(t *testing.T) {
	logger := &fakeLogger{}
	s := &srv{logger: logger}
	h := &Server{s: s}

	h.Wait() // nothing pending; marks the server closing

	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	s.logRequest("after close", 1, 0, "ok") // must not panic

	if got := logger.calls.Load(); got != 0 {
		t.Errorf("logger.calls = %d, want 0 (rejected while closing, never reaches InsertRequestLog)", got)
	}
	if !strings.Contains(buf.String(), "closing") {
		t.Errorf("log output = %q, want a rejection message mentioning the server is closing", buf.String())
	}
}
