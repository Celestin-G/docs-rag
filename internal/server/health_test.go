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
)

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

// pingerFunc adapts a func to pinger, so a test can control exactly when
// Ping returns (e.g. block until a channel closes, or until ctx is done).
type pingerFunc func(context.Context) error

func (f pingerFunc) Ping(ctx context.Context) error { return f(ctx) }

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

func TestReadyzRespectsTimeout(t *testing.T) {
	unblock := make(chan struct{}) // never closed
	s := &srv{ping: pingerFunc(func(ctx context.Context) error {
		select {
		case <-unblock:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})}
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
// what the analytics goroutine did without a real database, including
// whether the write got its own bounded, live context.
type fakeLogger struct {
	delay       time.Duration
	calls       atomic.Int32
	hadDeadline atomic.Bool
	ctxErr      atomic.Value
}

func (f *fakeLogger) InsertRequestLog(ctx context.Context, _ string, _, _ int, _ string) error {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= logWriteTimeout {
		f.hadDeadline.Store(true)
	}
	if err := ctx.Err(); err != nil {
		f.ctxErr.Store(err)
	}
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

// TestLogRequestUsesDetachedBoundedContext: the write gets its own bounded,
// live context even when the request that triggered it is long gone.
func TestLogRequestUsesDetachedBoundedContext(t *testing.T) {
	logger := &fakeLogger{}
	s := &srv{logger: logger}
	s.logRequest("q", 1, 0, "cancelled")
	(&Server{s: s}).Wait()

	if logger.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", logger.calls.Load())
	}
	if !logger.hadDeadline.Load() {
		t.Error("log write ctx has no deadline <= logWriteTimeout")
	}
	if v := logger.ctxErr.Load(); v != nil {
		t.Errorf("log write ctx already done: %v", v)
	}
}

// TestLogRequestConcurrentWithWait: every logRequest call racing Wait must
// be either recorded or rejected, never panic (run with -race), and never
// write after Wait has returned.
func TestLogRequestConcurrentWithWait(t *testing.T) {
	for iter := 0; iter < 5; iter++ {
		logger := &fakeLogger{}
		s := &srv{logger: logger}
		h := &Server{s: s}
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.logRequest("q", 1, 0, "ok")
			}()
		}
		h.Wait()
		before := logger.calls.Load()
		wg.Wait()
		time.Sleep(5 * time.Millisecond)
		if got := logger.calls.Load(); got != before {
			t.Fatalf("iter %d: %d request-log write(s) ran after Wait returned", iter, got-before)
		}
	}
}
