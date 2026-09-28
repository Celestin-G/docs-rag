package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

// TestRequestLogStatus covers the client-disconnect reclassification: a
// context.Canceled anywhere in the stage chain — however it got wrapped —
// is logged as "cancelled" rather than folded into res.Status, since that
// status came from a pipeline that never got to finish, not one that failed.
func TestRequestLogStatus(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status string
		want   string
	}{
		{name: "no error: status passes through unchanged", err: nil, status: "error", want: "error"},
		{name: "no error, ok status: passes through unchanged", err: nil, status: "ok", want: "ok"},
		{name: "bare context.Canceled overrides status", err: context.Canceled, status: "error", want: "cancelled"},
		{name: "context.Canceled wrapped by stageErr overrides status", err: &stageErr{Stage: "Search failed", Err: context.Canceled}, status: "error", want: "cancelled"},
		{name: "context.Canceled wrapped by fmt.Errorf %w overrides status", err: fmt.Errorf("ollama embed: %w", context.Canceled), status: "error", want: "cancelled"},
		{name: "an unrelated error leaves status unchanged", err: errors.New("boom"), status: "error", want: "error"},
		{name: "context.DeadlineExceeded is not treated as a cancellation", err: context.DeadlineExceeded, status: "error", want: "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestLogStatus(tc.err, tc.status); got != tc.want {
				t.Errorf("requestLogStatus(%v, %q) = %q, want %q", tc.err, tc.status, got, tc.want)
			}
		})
	}
}

func TestSourceURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		section string
		want    string
	}{
		{name: "page", file: "apps/docs/content/pages/modules/ledger/quick-start.mdx", want: "https://docs.formance.com/modules/ledger/quick-start"},
		{name: "nested page", file: "apps/docs/content/pages/modules/ledger/working-with/ledger-schema.mdx", want: "https://docs.formance.com/modules/ledger/working-with/ledger-schema"},
		{name: "index page drops the trailing segment", file: "apps/docs/content/pages/modules/numscript/index.mdx", want: "https://docs.formance.com/modules/numscript"},
		{name: "markdown extension", file: "apps/docs/content/pages/getting-started/connect-app.md", want: "https://docs.formance.com/getting-started/connect-app"},
		{name: "fragment has no standalone page", file: "apps/docs/content/fragments/ledger/api-routes.mdx", want: ""},
		{name: "heading becomes an anchor", file: "apps/docs/content/pages/modules/ledger/quick-start.mdx", section: "Creating a transaction", want: "https://docs.formance.com/modules/ledger/quick-start#creating-a-transaction"},
		{name: "punctuation and code ticks are dropped, not hyphenated", file: "apps/docs/content/pages/modules/numscript/reference/oneof.mdx", section: "Using `oneof` on the source side?", want: "https://docs.formance.com/modules/numscript/reference/oneof#using-oneof-on-the-source-side"},
		{name: "a generated dataset label is not a heading", file: "apps/docs/content/pages/modules/ledger/quick-start.mdx", section: "api.operations › v2CreateLedger", want: "https://docs.formance.com/modules/ledger/quick-start"},
		{name: "unrelated path", file: "README.md", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceURL(tc.file, tc.section); got != tc.want {
				t.Errorf("sourceURL(%q, %q) = %q, want %q", tc.file, tc.section, got, tc.want)
			}
		})
	}
}

func TestRenderSources(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources []source
		want    string
	}{
		{name: "no sources", sources: nil, want: ""},
		{
			// Same page twice: numbering must follow position, not identity,
			// because the model cites [i+1] for the i-th excerpt it was given.
			// With anchors the two entries also resolve to different places on
			// that page, which is the whole reason not to deduplicate them.
			name: "same page twice numbers by position, not identity",
			sources: []source{
				{File: "apps/docs/content/fragments/ledger/api-routes.mdx"},
				{File: "apps/docs/content/pages/modules/ledger/quick-start.mdx", Section: "Your First Transaction"},
				{File: "apps/docs/content/pages/modules/ledger/quick-start.mdx", Section: "Splitting Funds"},
			},
			want: "\n\n---\nSources:\n" +
				"[1] apps/docs/content/fragments/ledger/api-routes.mdx\n" +
				"[2] Your First Transaction — https://docs.formance.com/modules/ledger/quick-start#your-first-transaction\n" +
				"[3] Splitting Funds — https://docs.formance.com/modules/ledger/quick-start#splitting-funds\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := renderSources(tc.sources); got != tc.want {
				t.Errorf("renderSources mismatch\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// fakeUpstreams starts a fake OpenAI-compatible LLM and a fake Ollama embed
// endpoint for newTestSrv. It records the text the embed endpoint received,
// and — if entered is non-nil — signals it as soon as either endpoint sees a
// request, so a test can synchronise on "the pipeline reached the upstream"
// without sleeping.
type fakeUpstreams struct {
	mu         sync.Mutex
	embedInput []string
	entered    chan struct{}
}

func (fu *fakeUpstreams) signalEntered() {
	select {
	case fu.entered <- struct{}{}:
	default:
	}
}

// newTestSrv builds a *srv wired to fakeUpstreams' fake LLM/embed servers.
// translateHang / embedHang make the respective stage hang until the test
// ends (release fires in Cleanup) instead of answering; the embed endpoint
// otherwise answers HTTP 500, so answer() stops before the (DB-backed)
// search stage.
func newTestSrv(t *testing.T, translateHang, embedHang bool, fu *fakeUpstreams) *srv {
	t.Helper()
	release := make(chan struct{})
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // lets the server notice the client hanging up
		fu.signalEntered()
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
		fu.signalEntered()
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

// TestAnswerTimeouts covers the pipeline's timeout/cancellation contract:
// each stage timeout is independent, an expired one never leaks into the
// next stage, the total budget bounds every stage regardless of their own
// timeouts, and only a real caller cancel is logged as "cancelled".
func TestAnswerTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name          string
		translateHang bool
		embedHang     bool
		configure     func(s *srv)
		withCtx       func() (context.Context, context.CancelFunc)
		checkErr      func(t *testing.T, err error)
		wantStatus    string
		wantEmbedIn   string // sole recorded embed input; "" skips the check
	}{
		{
			name:          "translate timeout falls back to the original question without cancelling the parent",
			translateHang: true,
			configure:     func(s *srv) { s.translateTimeout = 100 * time.Millisecond },
			checkErr: func(t *testing.T, err error) {
				if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
					t.Fatalf("err = %v, want embed-stage HTTP 500 (parent ctx must still be live)", err)
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("err = %v leaked the translate deadline into the embed stage", err)
				}
			},
			wantStatus:  "error",
			wantEmbedIn: "search_query: Qu'est-ce qu'un ledger ?",
		},
		{
			// Total budget stays at newTestSrv's generous default so only
			// embedTimeout can fire.
			name:      "embed timeout is DeadlineExceeded mapped to status error, not cancelled",
			embedHang: true,
			configure: func(s *srv) { s.embedTimeout = 100 * time.Millisecond },
			checkErr: func(t *testing.T, err error) {
				var se *stageErr
				if !errors.As(err, &se) || se.Stage != "Embedding failed" {
					t.Fatalf("err = %v, want stageErr{Embedding failed}", err)
				}
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
				}
			},
			wantStatus:  "error",
			wantEmbedIn: "search_query: translated question",
		},
		{
			name:          "total budget bounds every stage even when per-stage timeouts are longer",
			translateHang: true,
			embedHang:     true,
			configure:     func(s *srv) { s.totalAnswerBudget = 200 * time.Millisecond },
			checkErr: func(t *testing.T, err error) {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want DeadlineExceeded from the total budget", err)
				}
			},
			wantStatus: "error",
		},
		{
			name:      "a caller cancel mid-pipeline is logged as cancelled",
			embedHang: true,
			withCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				time.AfterFunc(100*time.Millisecond, cancel)
				return ctx, cancel
			},
			wantStatus: "cancelled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fu := &fakeUpstreams{}
			s := newTestSrv(t, tc.translateHang, tc.embedHang, fu)
			if tc.configure != nil {
				tc.configure(s)
			}
			ctx := context.Background()
			if tc.withCtx != nil {
				var cancel context.CancelFunc
				ctx, cancel = tc.withCtx()
				defer cancel()
			}

			start := time.Now()
			res, err := s.answer(ctx, askReq{Question: "Qu'est-ce qu'un ledger ?", TopK: 5}, &strings.Builder{})
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("answer took %s, timeout not applied", elapsed)
			}
			if tc.checkErr != nil {
				tc.checkErr(t, err)
			}
			if got := requestLogStatus(err, res.Status); got != tc.wantStatus {
				t.Errorf("logged status = %q, want %q", got, tc.wantStatus)
			}
			if tc.wantEmbedIn != "" {
				fu.mu.Lock()
				defer fu.mu.Unlock()
				if len(fu.embedInput) != 1 || fu.embedInput[0] != tc.wantEmbedIn {
					t.Errorf("embed input = %q, want [%q]", fu.embedInput, tc.wantEmbedIn)
				}
			}
		})
	}
}

// Probes through the real mux: neither /healthz nor /readyz shares the
// ask/feedback rate budget, and neither writes to request_logs — both are
// infrastructure plumbing an orchestrator polls, not documentation traffic.
func TestProbesThroughMuxNotRateLimitedNorLogged(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")
	h, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	logger := &fakeLogger{}
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
