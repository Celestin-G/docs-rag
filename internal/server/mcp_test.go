package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func handlerCalled(t *testing.T) (http.Handler, *bool) {
	t.Helper()
	called := false
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}), &called
}

func TestWithMCPAuth(t *testing.T) {
	for _, tc := range []struct {
		name       string
		token      string // MCP_TOKEN configured on the server
		authHeader string // Authorization header sent by the client
		wantStatus int
		wantCalled bool
	}{
		{
			name:       "token unset: request passes through unauthenticated",
			token:      "",
			authHeader: "",
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "token set, correct bearer header: passes",
			token:      "s3cr3t",
			authHeader: "Bearer s3cr3t",
			wantStatus: http.StatusOK,
			wantCalled: true,
		},
		{
			name:       "token set, missing header: 401, handler not called",
			token:      "s3cr3t",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
			wantCalled: false,
		},
		{
			name:       "token set, wrong token: 401, handler not called",
			token:      "s3cr3t",
			authHeader: "Bearer wrong",
			wantStatus: http.StatusUnauthorized,
			wantCalled: false,
		},
		{
			name:       "token set, non-bearer scheme: 401, handler not called",
			token:      "s3cr3t",
			authHeader: "Basic czNjcjN0",
			wantStatus: http.StatusUnauthorized,
			wantCalled: false,
		},
		{
			name:       "token set, empty bearer value: 401, handler not called",
			token:      "s3cr3t",
			authHeader: "Bearer ",
			wantStatus: http.StatusUnauthorized,
			wantCalled: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &srv{mcpToken: tc.token}
			next, called := handlerCalled(t)

			r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
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

// TestWithMCPAuthAppliesToGET covers the streamable-HTTP GET stream, not
// just POST tool calls: withMCPAuth must not special-case the method, or a
// client could open the server-to-client stream unauthenticated even with
// MCP_TOKEN configured.
func TestWithMCPAuthAppliesToGET(t *testing.T) {
	s := &srv{mcpToken: "s3cr3t"}
	next, called := handlerCalled(t)

	r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	w := httptest.NewRecorder()

	s.withMCPAuth(next).ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/mcp without a token: status = %d, want 401", w.Code)
	}
	if *called {
		t.Error("handler should not be called for an unauthenticated GET")
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

func TestWithMCPRateLimit(t *testing.T) {
	s := &srv{mcpLimiter: newRateLimiter(mcpRatePerMinute, mcpRateBurst)}
	next, called := handlerCalled(t)
	handler := s.withMCPRateLimit(next)

	// Burst allowance: the first mcpRateBurst requests from the same IP succeed.
	for i := 0; i < mcpRateBurst; i++ {
		*called = false
		r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
		r.RemoteAddr = "203.0.113.5:1234"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want %d", i, w.Code, http.StatusOK)
		}
		if !*called {
			t.Fatalf("request %d: handler was not called", i)
		}
	}

	// One more request exceeds the burst and must be rejected without
	// reaching the handler.
	*called = false
	r := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
	r.RemoteAddr = "203.0.113.5:1234"
	w := httptest.NewRecorder()

	handler.ServeHTTP(w, r)

	if w.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", w.Code, http.StatusTooManyRequests)
	}
	if *called {
		t.Error("handler was called on a rate-limited request")
	}

	// A different client IP is unaffected — it gets its own bucket.
	*called = false
	r2 := httptest.NewRequest(http.MethodPost, "/api/mcp", nil)
	r2.RemoteAddr = "198.51.100.9:1234"
	w2 := httptest.NewRecorder()

	handler.ServeHTTP(w2, r2)

	if w2.Code != http.StatusOK {
		t.Errorf("different IP: status = %d, want %d", w2.Code, http.StatusOK)
	}
	if !*called {
		t.Error("different IP: handler was not called")
	}
}
