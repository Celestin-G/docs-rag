package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

// TestHealthzIsNotRateLimited proves /healthz is reachable through the mux
// New() wires up even after the /api/ask + /api/feedback budget for the same
// IP is exhausted — an orchestrator polling every few seconds must never be
// throttled by the same bucket as anonymous browser traffic.
func TestHealthzIsNotRateLimited(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")
	handler, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	ip := "203.0.113.80:1234"
	for i := 0; i < rateBurst; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
		r.RemoteAddr = ip
		handler.ServeHTTP(httptest.NewRecorder(), r)
	}
	// Confirm the ask/feedback budget really is exhausted, so a pass below
	// would catch a regression rather than passing vacuously.
	exhaustCheck := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	exhaustCheck.RemoteAddr = ip
	exhaustW := httptest.NewRecorder()
	handler.ServeHTTP(exhaustW, exhaustCheck)
	if exhaustW.Code != http.StatusTooManyRequests {
		t.Fatalf("setup: /api/ask budget not exhausted, status = %d", exhaustW.Code)
	}

	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("/healthz status = %d, want %d (must not share the ask/feedback rate budget)", w.Code, http.StatusOK)
	}
}

// TestHealthz pins down the liveness contract: always 200, no dependency on
// anything (there is no store on this srv at all), so a DB outage never
// makes an orchestrator kill an otherwise-healthy process.
func TestHealthz(t *testing.T) {
	s := &srv{}
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	s.handleHealthz(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := w.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// fakePinger lets TestReadyz simulate a failing database without standing
// one up.
type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestReadyz(t *testing.T) {
	for _, tc := range []struct {
		name       string
		pingErr    error
		wantStatus int
	}{
		{name: "database reachable", pingErr: nil, wantStatus: http.StatusOK},
		{name: "database ping fails", pingErr: errors.New("connection refused"), wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &srv{ping: fakePinger{err: tc.pingErr}}
			r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			w := httptest.NewRecorder()

			s.handleReadyz(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
		})
	}
}

// slowPinger blocks until unblocked, letting TestReadyzRespectsTimeout prove
// the ping is actually bounded by readyzTimeout rather than able to hang the
// probe forever.
type slowPinger struct{ unblock chan struct{} }

func (p slowPinger) Ping(ctx context.Context) error {
	select {
	case <-p.unblock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestReadyzRespectsTimeout(t *testing.T) {
	s := &srv{ping: slowPinger{unblock: make(chan struct{})}} // never unblocked
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	// Give the request itself a deadline well past readyzTimeout, so a pass
	// here proves handleReadyz imposes its own bound rather than merely
	// inheriting one from the caller.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()

	start := time.Now()
	s.handleReadyz(w, r)
	elapsed := time.Since(start)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
	if elapsed > readyzTimeout+time.Second {
		t.Errorf("handleReadyz took %s, want roughly readyzTimeout (%s)", elapsed, readyzTimeout)
	}
}

// fakeLogger records writes (after an optional delay) so tests can observe
// what the analytics goroutine did without a real database.
type fakeLogger struct {
	delay time.Duration
	calls atomic.Int32
}

func (f *fakeLogger) InsertRequestLog(context.Context, string, int, int, string) error {
	time.Sleep(f.delay)
	f.calls.Add(1)
	return nil
}

// TestServerWaitBlocksUntilLogWritesFinish pins down the graceful-shutdown
// contract: (*Server).Wait must not return before every request-log write
// launched by logRequest has actually completed, so cmd/serve.go can safely
// close the DB pool right after it returns.
func TestServerWaitBlocksUntilLogWritesFinish(t *testing.T) {
	logger := &fakeLogger{delay: 50 * time.Millisecond}
	s := &srv{logger: logger}
	h := &Server{s: s}

	const n = 5
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.logRequest("question", 1, 1, "ok")
		}()
	}
	wg.Wait() // all n writes launched (logWG.Add happened) before we wait on it

	h.Wait()

	if got := logger.calls.Load(); got != n {
		t.Errorf("logger.calls = %d, want %d (Wait returned before every write finished)", got, n)
	}
}

// TestServerWaitReturnsImmediatelyWithNoPendingWrites guards against a
// trivial but easy mistake (e.g. an Add without a matching Done firing on
// the zero-value WaitGroup) that would make Wait hang forever with nothing
// in flight.
func TestServerWaitReturnsImmediatelyWithNoPendingWrites(t *testing.T) {
	h := &Server{s: &srv{}}

	done := make(chan struct{})
	go func() {
		h.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Wait() did not return with no pending writes")
	}
}
