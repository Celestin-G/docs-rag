package server

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestStreamCancelOnShutdown pins down the fix for the MCP GET stream
// blocking a graceful shutdown: a handler wrapped in withStreamCancel that
// blocks on its request context (exactly what the go-sdk's streamable HTTP
// serveGET does) must be released as soon as streamCtx is cancelled — the
// signal (*Server).CancelStreams sends the instant shutdown begins — rather
// than only when http.Server.Shutdown's own deadline elapses.
func TestStreamCancelOnShutdown(t *testing.T) {
	s := &srv{}
	s.streamCtx, s.cancelStreams = context.WithCancel(context.Background())

	entered := make(chan struct{})
	handler := s.withStreamCancel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done() // mirrors serveGET: blocks until the request ctx is done
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv := &http.Server{Handler: handler}
	go httpSrv.Serve(ln)
	t.Cleanup(func() { httpSrv.Close() })

	clientErrCh := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			clientErrCh <- err
			return
		}
		resp.Body.Close()
		clientErrCh <- nil
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler was never entered")
	}

	// The signal cmd/serve.go sends via httpSrv.RegisterOnShutdown(handler.CancelStreams).
	s.cancelStreams()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Shutdown took %s, want it to return quickly once streamCtx is cancelled", elapsed)
	}
	if err := <-clientErrCh; err != nil {
		t.Errorf("client request errored: %v", err)
	}
}

// TestStreamCancelLeavesOtherRequestsUnaffected guards the other half of the
// same fix: a request that finishes on its own — the common case, and what
// every /api/ask or MCP POST call does — must not be disturbed by
// withStreamCancel, and must not leak the goroutine context.AfterFunc parks
// on streamCtx once it returns.
func TestStreamCancelLeavesOtherRequestsUnaffected(t *testing.T) {
	s := &srv{}
	s.streamCtx, s.cancelStreams = context.WithCancel(context.Background())
	t.Cleanup(s.cancelStreams)

	handler := s.withStreamCancel(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.Context().Err(); err != nil {
			t.Errorf("request ctx already done: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	httpSrv := &http.Server{Handler: handler}
	go httpSrv.Serve(ln)
	t.Cleanup(func() { httpSrv.Close() })

	resp, err := http.Get("http://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestWaitHandlersBoundedByTimeout pins down WaitHandlers' two contracts:
// it returns false while a tracked handler (see (*Server).ServeHTTP) is
// still running, bounded by the timeout passed in rather than blocking
// forever, and true once every handler has actually returned.
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

// TestWaitHandlersNoPendingRequests guards against the same easy WaitGroup
// mistake as TestServerWaitReturnsImmediatelyWithNoPendingWrites, for
// handlersWG this time: WaitHandlers must not hang when nothing is in flight.
func TestWaitHandlersNoPendingRequests(t *testing.T) {
	h := &Server{s: &srv{}}
	if !h.WaitHandlers(time.Second) {
		t.Fatal("WaitHandlers returned false with no pending handlers")
	}
}

// TestLogRequestAfterClosingDoesNotPanicAndLogsRejection covers the
// Add-after-Wait-begins race the closing flag exists to prevent: once
// (*Server).Wait has started draining logWG, a later logRequest call must
// neither panic (sync.WaitGroup misuse) nor silently vanish — it is rejected,
// and the rejection itself is logged rather than dropped without a trace.
func TestLogRequestAfterClosingDoesNotPanicAndLogsRejection(t *testing.T) {
	logger := &countingLogger{}
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

// TestLogRequestBeforeClosingStillRuns is the control for the test above:
// a write started before Wait begins must complete normally, not be
// mistakenly rejected too.
func TestLogRequestBeforeClosingStillRuns(t *testing.T) {
	logger := &countingLogger{}
	s := &srv{logger: logger}
	h := &Server{s: s}

	s.logRequest("before close", 1, 0, "ok")
	h.Wait()

	if got := logger.calls.Load(); got != 1 {
		t.Errorf("logger.calls = %d, want 1", got)
	}
}
