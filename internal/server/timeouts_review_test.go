package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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

// fakeUpstreams starts a fake OpenAI-compatible LLM and a fake Ollama embed
// endpoint. translateDelay / embedHang control how each behaves; the embed
// endpoint records the text it received and answers HTTP 500 so answer()
// stops before the (DB-backed) search stage.
type fakeUpstreams struct {
	mu         sync.Mutex
	embedInput []string
}

func newTestSrv(t *testing.T, translateHang, embedHang bool, fu *fakeUpstreams) *srv {
	t.Helper()
	release := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		if translateHang {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"translated question"}}]}`))
	}))
	t.Cleanup(llmSrv.Close)
	embSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.Copy(io.Discard, r.Body)
		fu.mu.Lock()
		fu.embedInput = append(fu.embedInput, body.Input...)
		fu.mu.Unlock()
		if embedHang {
			select {
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(embSrv.Close)
	t.Cleanup(func() { close(release) }) // runs first (LIFO)

	t.Setenv("LLM_BASE_URL", llmSrv.URL+"/v1")
	t.Setenv("OLLAMA_HOST", embSrv.URL)
	return &srv{
		llm:               llm.New(),
		emb:               embedder.New(),
		translateTimeout:  10 * time.Second,
		embedTimeout:      10 * time.Second,
		searchTimeout:     10 * time.Second,
		totalAnswerBudget: 10 * time.Second,
	}
}

// A translate stage timeout must fall back to the ORIGINAL question and must
// not cancel the parent context: the embed stage still runs (and here fails
// on its own HTTP 500, not on a cancelled context).
func TestAnswerTranslateTimeoutFallsBackWithoutCancellingParent(t *testing.T) {
	fu := &fakeUpstreams{}
	s := newTestSrv(t, true, false, fu)
	s.translateTimeout = 100 * time.Millisecond

	start := time.Now()
	res, err := s.answer(context.Background(), askReq{Question: "Qu'est-ce qu'un ledger ?", TopK: 5}, &strings.Builder{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("answer took %s, translate timeout not applied", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("err = %v, want embed-stage HTTP 500 (parent ctx must still be live)", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v leaked the translate deadline into the embed stage", err)
	}
	if res.Status != "error" {
		t.Errorf("status = %q, want error", res.Status)
	}
	fu.mu.Lock()
	defer fu.mu.Unlock()
	if len(fu.embedInput) != 1 || fu.embedInput[0] != "search_query: Qu'est-ce qu'un ledger ?" {
		t.Errorf("embed input = %q, want the original question as fallback", fu.embedInput)
	}
}

// An embed stage timeout is a DeadlineExceeded mapped to status "error" and a
// stageErr, never to "cancelled".
func TestAnswerEmbedTimeoutIsErrorNotCancelled(t *testing.T) {
	fu := &fakeUpstreams{}
	s := newTestSrv(t, false, true, fu)
	s.embedTimeout = 100 * time.Millisecond

	start := time.Now()
	res, err := s.answer(context.Background(), askReq{Question: "what is a ledger", TopK: 5}, &strings.Builder{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("answer took %s, embed timeout not applied", elapsed)
	}
	var se *stageErr
	if !errors.As(err, &se) || se.Stage != "Embedding failed" {
		t.Fatalf("err = %v, want stageErr{Embedding failed}", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if got := requestLogStatus(err, res.Status); got != "error" {
		t.Errorf("logged status = %q, want error (a stage timeout is not a client cancel)", got)
	}
	fu.mu.Lock()
	defer fu.mu.Unlock()
	if len(fu.embedInput) != 1 || fu.embedInput[0] != "search_query: translated question" {
		t.Errorf("embed input = %q, want translated question", fu.embedInput)
	}
}

// The total budget caps every stage even when the per-stage timeouts are
// longer than the budget itself.
func TestAnswerTotalBudgetBoundsStages(t *testing.T) {
	fu := &fakeUpstreams{}
	s := newTestSrv(t, true, true, fu)
	s.totalAnswerBudget = 200 * time.Millisecond

	start := time.Now()
	res, err := s.answer(context.Background(), askReq{Question: "q", TopK: 5}, &strings.Builder{})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("answer took %s, total budget not applied", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded from the total budget", err)
	}
	if got := requestLogStatus(err, res.Status); got != "error" {
		t.Errorf("logged status = %q, want error", got)
	}
}

// A caller (client) cancel mid-pipeline is logged as "cancelled".
func TestAnswerParentCancelIsCancelled(t *testing.T) {
	fu := &fakeUpstreams{}
	s := newTestSrv(t, false, true, fu)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	res, err := s.answer(ctx, askReq{Question: "q", TopK: 5}, &strings.Builder{})
	if got := requestLogStatus(err, res.Status); got != "cancelled" {
		t.Errorf("logged status = %q (err %v), want cancelled", got, err)
	}
}

// Zero or negative overrides produce an already-expired deadline: every
// /api/ask would fail instantly. They should be rejected like malformed
// values (envDuration's own doc says it guards against a "zero
// (immediately-expired) deadline").
func TestEnvDurationRejectsNonPositive(t *testing.T) {
	const def = 7 * time.Second
	for _, v := range []string{"0", "0s", "-1s"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("TEST_ENV_DURATION_NP", v)
			if got := envDuration("TEST_ENV_DURATION_NP", def); got != def {
				t.Errorf("envDuration(%q) = %s, want default %s", v, got, def)
			}
		})
	}
}

// countingLogger counts InsertRequestLog calls and records the context it got.
type countingLogger struct {
	calls       atomic.Int32
	hadDeadline atomic.Bool
	ctxErr      atomic.Value
}

func (c *countingLogger) InsertRequestLog(ctx context.Context, _ string, _, _ int, _ string) error {
	c.calls.Add(1)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) <= logWriteTimeout {
		c.hadDeadline.Store(true)
	}
	if err := ctx.Err(); err != nil {
		c.ctxErr.Store(err)
	}
	return nil
}

// Probes through the real mux: /readyz is not rate-limited either, and
// neither probe writes to request_logs.
func TestProbesThroughMuxNotRateLimitedNorLogged(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")
	h, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	logger := &countingLogger{}
	h.s.logger = logger
	h.s.ping = fakePinger{}

	ip := "203.0.113.90:1234"
	for i := 0; i < rateBurst+5; i++ {
		for _, path := range []string{"/healthz", "/readyz"} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.RemoteAddr = ip
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("%s #%d status = %d, want 200", path, i, w.Code)
			}
		}
	}
	h.Wait()
	if n := logger.calls.Load(); n != 0 {
		t.Errorf("probes wrote %d request logs, want 0", n)
	}
}

// Probe endpoints only answer GET.
func TestProbesRejectNonGET(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")
	h, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 405", path, w.Code)
		}
	}
}

// /readyz with a request whose context is already cancelled reports 503
// promptly rather than 200.
func TestReadyzCancelledRequest(t *testing.T) {
	s := &srv{ping: slowPinger{unblock: make(chan struct{})}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodGet, "/readyz", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	start := time.Now()
	s.handleReadyz(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("readyz did not return promptly on a cancelled request")
	}
}

// The request-log write gets its own bounded, live context even when the
// request that triggered it is long gone.
func TestLogRequestUsesDetachedBoundedContext(t *testing.T) {
	logger := &countingLogger{}
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
