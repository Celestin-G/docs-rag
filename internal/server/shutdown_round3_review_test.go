package server

// Round 3 review tests: enterHandler/WaitHandlers admission, ForceCancel
// semantics, graceful-phase behaviour of in-flight keep-alive requests, and
// per-call goroutine hygiene of askDocs.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A request that is already in flight when Shutdown begins must complete
// normally (200), not get 503: WaitHandlers only runs after Shutdown returns.
func TestReviewInFlightRequestDuringGracefulShutdownIsNot503(t *testing.T) {
	t.Setenv("MCP_TOKEN", "tok")
	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := &http.Server{Handler: h}
	go httpSrv.Serve(ln)
	t.Cleanup(func() { httpSrv.Close() })

	status := make(chan int, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/readyz")
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

type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

// Concurrent requests racing WaitHandlers: each is either admitted (and then
// WaitHandlers must not report done before it returns) or refused with 503
// without ever reaching the mux. Run under -race.
func TestReviewEnterHandlerRacesWaitHandlers(t *testing.T) {
	for iter := 0; iter < 30; iter++ {
		var running, served atomic.Int32
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			running.Add(1)
			time.Sleep(2 * time.Millisecond)
			served.Add(1)
			running.Add(-1)
		})
		h := &Server{mux: mux, s: &srv{logger: &countingLogger{}}}
		var wg sync.WaitGroup
		var refused atomic.Int32
		const n = 40
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				w := &codeRecorder{h: http.Header{}}
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
				if w.code == http.StatusServiceUnavailable {
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

// codeRecorder is a minimal, goroutine-local ResponseWriter.
type codeRecorder struct {
	h    http.Header
	code int
}

func (c *codeRecorder) Header() http.Header         { return c.h }
func (c *codeRecorder) Write(p []byte) (int, error) { return len(p), nil }
func (c *codeRecorder) WriteHeader(code int)        { c.code = code }

// A tool call refused by enterHandler must not log a request (it never ran
// the pipeline) and must not touch handlersWG.
func TestReviewRefusedToolCallDoesNotLog(t *testing.T) {
	logger := &fakeLogger{}
	h := &Server{mux: http.NewServeMux(), s: &srv{logger: logger}}
	h.WaitHandlers(time.Second)
	for i := 0; i < 5; i++ {
		if _, _, err := h.s.askDocs(context.Background(), nil, askDocsInput{Question: "q"}); !errors.Is(err, errShuttingDown) {
			t.Fatalf("err = %v, want errShuttingDown", err)
		}
	}
	h.Wait()
	if got := logger.calls.Load(); got != 0 {
		t.Errorf("refused tool calls logged %d request(s), want 0", got)
	}
	if !h.WaitHandlers(100 * time.Millisecond) {
		t.Error("refused tool calls left handlersWG non-zero")
	}
}

// ForceCancel must be idempotent (runServe calls it inline AND via defer).
func TestReviewForceCancelIdempotent(t *testing.T) {
	t.Setenv("MCP_TOKEN", "tok")
	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.ForceCancel()
	h.ForceCancel()
	h.CancelStreams()
	h.CancelStreams()
}

// No goroutine leak per MCP call: many askDocs calls (validation-failing and
// successful-admission) must not leave AfterFunc goroutines or ctx watchers
// behind.
func TestReviewAskDocsNoGoroutineLeak(t *testing.T) {
	s := &srv{logger: &countingLogger{}}
	s.hardStop, s.forceCancel = context.WithCancel(context.Background())
	defer s.forceCancel()
	runtime.GC()
	before := runtime.NumGoroutine()
	for i := 0; i < 500; i++ {
		_, _, err := s.askDocs(context.Background(), nil, askDocsInput{Question: "   "})
		if err == nil {
			t.Fatal("empty question accepted")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Errorf("goroutines grew from %d to %d over 500 askDocs calls", before, after)
	}
	// hardStop must still be live and have no lingering AfterFunc: cancelling
	// it now must not panic or affect anything.
	h := &Server{s: s}
	if !h.WaitHandlers(time.Second) {
		t.Error("handlersWG not back to zero after 500 calls")
	}
}

// WaitHandlers timing out leaves its waiter goroutine parked; a second call
// must still work and report true once the handler finishes.
func TestReviewWaitHandlersTwiceAfterTimeout(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(http.ResponseWriter, *http.Request) { close(entered); <-release })
	h := &Server{mux: mux, s: &srv{}}
	go h.ServeHTTP(&codeRecorder{h: http.Header{}}, httptest.NewRequest(http.MethodGet, "/", nil))
	<-entered
	if h.WaitHandlers(50 * time.Millisecond) {
		t.Fatal("WaitHandlers true while handler running")
	}
	close(release)
	if !h.WaitHandlers(time.Second) {
		t.Error("second WaitHandlers did not report true after handler finished")
	}
}
