package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAskDocsRequiresQuestion covers the empty-question guard in askDocs: a
// question that is empty, or only whitespace, is rejected before it ever
// reaches the pipeline (see maxQuestionLen's sibling check right below it).
// A bare &srv{} is the cheapest server that satisfies askDocs here — the
// question check runs before hardStop or the pipeline are ever touched, so
// neither needs to be initialised (unlike TestAskDocsTrackedAndStoppedByForceCancel
// in shutdown_test.go, which exercises those and so uses newTestSrv).
func TestAskDocsRequiresQuestion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		question string
	}{
		{name: "empty", question: ""},
		{name: "whitespace only", question: "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &srv{}
			_, _, err := s.askDocs(context.Background(), nil, askDocsInput{Question: tc.question})
			if err == nil || !strings.Contains(err.Error(), "question is required") {
				t.Fatalf("askDocs(%q) err = %v, want an error containing %q", tc.question, err, "question is required")
			}
		})
	}
}

func handlerCalled(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	called := false
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}), &called
}

// TestWithMCPAuth covers both POST (tool calls) and GET (the streamable-HTTP
// stream): withMCPAuth must not special-case the method, or a client could
// open the server-to-client stream unauthenticated even with MCP_TOKEN set.
func TestWithMCPAuth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		method     string // defaults to POST
		token      string // MCP_TOKEN configured on the server
		authHeader string // Authorization header sent by the client
		wantStatus int
		wantCalled bool
	}{
		{name: "token unset: request passes through unauthenticated", token: "", authHeader: "", wantStatus: http.StatusOK, wantCalled: true},
		{name: "token set, correct bearer header: passes", token: "s3cr3t", authHeader: "Bearer s3cr3t", wantStatus: http.StatusOK, wantCalled: true},
		{name: "token set, missing header: 401, handler not called", token: "s3cr3t", authHeader: "", wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "token set, wrong token: 401, handler not called", token: "s3cr3t", authHeader: "Bearer wrong", wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "token set, non-bearer scheme: 401, handler not called", token: "s3cr3t", authHeader: "Basic czNjcjN0", wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "token set, empty bearer value: 401, handler not called", token: "s3cr3t", authHeader: "Bearer ", wantStatus: http.StatusUnauthorized, wantCalled: false},
		{name: "GET stream without a token: 401, handler not called", method: http.MethodGet, token: "s3cr3t", authHeader: "", wantStatus: http.StatusUnauthorized, wantCalled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			method := tc.method
			if method == "" {
				method = http.MethodPost
			}
			s := &srv{mcpToken: tc.token}
			next, called := handlerCalled(t)

			r := httptest.NewRequest(method, "/api/mcp", nil)
			if tc.authHeader != "" {
				r.Header.Set("Authorization", tc.authHeader)
			}
			w := httptest.NewRecorder()

			s.withMCPAuth(next).ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", w.Code, tc.wantStatus)
			}
			if *called != tc.wantCalled {
				t.Errorf("handler called = %v, want %v", *called, tc.wantCalled)
			}
			if tc.wantStatus == http.StatusUnauthorized {
				if got := w.Header().Get("WWW-Authenticate"); got == "" {
					t.Error("expected WWW-Authenticate header on 401, got none")
				}
			}
		})
	}
}

// TestMCPHandlerRateLimitWrapsAuth exercises the actual composition in
// mcpHandler() (withMCPRateLimit wraps withMCPAuth), not just each
// middleware in isolation: a wrong-token request must still consume the
// rate-limit budget, per the comment on mcpHandler ("Rate limit first, auth
// second"). If that ordering were ever flipped, an attacker could send
// unlimited wrong-token guesses without ever being rate-limited, since a
// 401 would be returned before the limiter saw the request.
func TestMCPHandlerRateLimitWrapsAuth(t *testing.T) {
	s := &srv{
		mcpToken:   "s3cr3t",
		mcpLimiter: newRateLimiter(mcpRatePerMinute, mcpRateBurst),
	}
	handler := s.mcpHandler()
	ip := "203.0.113.70:1234"

	for i := 0; i < mcpRateBurst; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
		r.RemoteAddr = ip
		r.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, r)

		if w.Code != http.StatusUnauthorized {
			t.Fatalf("request %d: status = %d, want 401 (still within the rate budget)", i, w.Code)
		}
	}

	// The burst is now exhausted by failed-auth attempts alone: the next
	// request must be rejected as rate-limited (429), proving the limiter
	// counted them.
	r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
	r.RemoteAddr = ip
	r.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429 (rate limit should count failed-auth attempts)", w.Code)
	}
}
