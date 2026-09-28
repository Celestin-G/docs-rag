package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

// askDocs is called with a context that never cancels, exactly like the
// go-sdk's notDone tool context. It must stay tracked by WaitHandlers while
// running, and ForceCancel alone must stop it.
func TestAskDocsTrackedAndStoppedByForceCancel(t *testing.T) {
	entered := make(chan struct{}, 1)
	llmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer llmSrv.Close()
	t.Setenv("LLM_BASE_URL", llmSrv.URL+"/v1")
	t.Setenv("MCP_TOKEN", "tok")

	h, err := New(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.s.logger = &countingLogger{}
	h.s.llm = llm.New()
	h.s.emb = embedder.New()
	h.s.translateTimeout = 8 * time.Second
	h.s.totalAnswerBudget = 9 * time.Second

	done := make(chan error, 1)
	go func() {
		_, _, err := h.s.askDocs(context.Background(), nil, askDocsInput{Question: "what is a ledger"})
		done <- err
	}()
	select {
	case <-entered:
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
}

// Once WaitHandlers has started draining, new work is refused instead of
// racing handlersWG.Wait: tool calls get a tool error, HTTP requests a 503.
func TestWorkRefusedAfterWaitHandlers(t *testing.T) {
	logger := &countingLogger{}
	h := &Server{mux: http.NewServeMux(), s: &srv{logger: logger}}
	if !h.WaitHandlers(time.Second) {
		t.Fatal("WaitHandlers returned false with nothing in flight")
	}

	t.Run("mcp tool call", func(t *testing.T) {
		_, _, err := h.s.askDocs(context.Background(), nil, askDocsInput{Question: "q"})
		if !errors.Is(err, errShuttingDown) {
			t.Errorf("askDocs err = %v, want %v", err, errShuttingDown)
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
