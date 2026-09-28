package server

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

func mustPrefixes(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	prefixes := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			t.Fatalf("ParsePrefix(%q): %v", c, err)
		}
		prefixes[i] = p
	}
	return prefixes
}

func TestClientIP(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		proxies    []netip.Prefix
		want       string
	}{
		{
			name:       "trusted proxy, single-hop XFF returns the address the proxy actually saw",
			remoteAddr: "127.0.0.1:5555",
			xff:        "9.9.9.9, 203.0.113.7", // 9.9.9.9 is whatever the client claimed
			proxies:    defaultProxies,
			want:       "203.0.113.7",
		},
		{
			name:       "trusted proxy, spoofed leftmost XFF entry is ignored",
			remoteAddr: "127.0.0.1:5555",
			xff:        "203.0.113.7, 198.51.100.9",
			proxies:    defaultProxies,
			want:       "198.51.100.9",
		},
		{
			name:       "trusted proxy, multi-hop XFF skips trusted hops",
			remoteAddr: "127.0.0.1:5555",
			xff:        "203.0.113.7, 10.0.0.9",
			proxies:    mustPrefixes(t, "127.0.0.1/32", "10.0.0.9/32"),
			want:       "203.0.113.7",
		},
		{
			name:       "untrusted RemoteAddr: XFF header ignored",
			remoteAddr: "8.8.8.8:1234",
			xff:        "203.0.113.7",
			proxies:    defaultProxies,
			want:       "8.8.8.8",
		},
		{
			name:       "untrusted RemoteAddr: X-Real-IP header ignored",
			remoteAddr: "8.8.8.8:1234",
			xri:        "203.0.113.7",
			proxies:    defaultProxies,
			want:       "8.8.8.8",
		},
		{
			name:       "trusted proxy, X-Real-IP used when no XFF",
			remoteAddr: "127.0.0.1:1234",
			xri:        "203.0.113.9",
			proxies:    defaultProxies,
			want:       "203.0.113.9",
		},
		{
			name:       "trusted proxy, missing headers falls back to RemoteAddr",
			remoteAddr: "127.0.0.1:1234",
			proxies:    defaultProxies,
			want:       "127.0.0.1",
		},
		{
			name:       "IPv6 trusted proxy with XFF",
			remoteAddr: "[::1]:8080",
			xff:        "2001:db8::1",
			proxies:    defaultProxies,
			want:       "2001:db8::1",
		},
		{
			name:       "IPv6 untrusted RemoteAddr falls back to RemoteAddr",
			remoteAddr: "[2001:db8::5]:8080",
			xff:        "2001:db8::1",
			proxies:    defaultProxies,
			want:       "2001:db8::5",
		},
		{
			name:       "malformed XFF entries are skipped",
			remoteAddr: "127.0.0.1:1234",
			xff:        "not-an-ip, 203.0.113.7",
			proxies:    defaultProxies,
			want:       "203.0.113.7",
		},
		{
			name:       "RemoteAddr without a port",
			remoteAddr: "127.0.0.1",
			proxies:    defaultProxies,
			want:       "127.0.0.1",
		},
		{
			name:       "XFF made only of trusted proxy addresses falls back to RemoteAddr",
			remoteAddr: "127.0.0.1:1234",
			xff:        "10.0.0.9",
			proxies:    mustPrefixes(t, "127.0.0.1/32", "10.0.0.9/32"),
			want:       "127.0.0.1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if tc.xri != "" {
				r.Header.Set("X-Real-IP", tc.xri)
			}
			if got := clientIP(r, tc.proxies); got != tc.want {
				t.Errorf("clientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestClientIPv4MappedIPv6 documents a gap in addrTrusted: netip.Prefix.Contains
// does not unmap IPv4-in-IPv6 addresses, so "::ffff:127.0.0.1" does NOT match
// the "127.0.0.1/32" prefix even though it is the same host. If the Go server
// ever sees RemoteAddr (or a proxy-vouched XFF hop) in this form — e.g. a
// dual-stack listener that preserves the mapped form, or an upstream proxy
// that writes addresses in mapped form — a proxy that should be trusted is
// treated as untrusted, and X-Forwarded-For / X-Real-IP are silently ignored
// even though the request did come from a trusted host.
//
// This currently FAILS: addrTrusted (and clientIP's callers) should call
// addr.Unmap() before matching against trustedProxies, so an IPv4-mapped
// form is recognised as equivalent to its plain IPv4 form.
func TestClientIPv4MappedIPv6(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "[::ffff:127.0.0.1]:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7")

	got := clientIP(r, defaultProxies)
	want := "203.0.113.7"
	if got != want {
		t.Errorf("clientIP() = %q, want %q (RemoteAddr ::ffff:127.0.0.1 should be recognised as trusted, "+
			"equivalent to 127.0.0.1 — see addrTrusted, missing netip.Addr.Unmap())", got, want)
	}
}

// TestClientIPv4MappedXFFHopIsUnmappedAndTrusted covers the other place the
// round-1 fix touched besides RemoteAddr: an intermediate XFF hop written in
// IPv4-mapped IPv6 form (e.g. "::ffff:10.0.0.9") must still be recognised as
// the same trusted proxy as its plain "10.0.0.9" form and skipped, not
// mistaken for the client. Before the fix, addrTrusted was only ever called
// with unmapped RemoteAddr; this pins down that the per-hop unmap in the XFF
// walk (clientIP's `unmappedAddr := addr.Unmap()`) actually runs too.
func TestClientIPv4MappedXFFHopIsUnmappedAndTrusted(t *testing.T) {
	proxies := mustPrefixes(t, "127.0.0.1/32", "10.0.0.9/32")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, ::ffff:10.0.0.9")

	got := clientIP(r, proxies)
	want := "203.0.113.7"
	if got != want {
		t.Errorf("clientIP() = %q, want %q (the mapped-form trusted hop ::ffff:10.0.0.9 should be "+
			"skipped just like its plain form, leaving the real client 203.0.113.7)", got, want)
	}
}

// TestClientIPv4MappedXRealIP covers X-Real-IP itself arriving in
// IPv4-mapped IPv6 form: the returned client IP must be the unmapped
// "203.0.113.9", not the raw "::ffff:203.0.113.9" string, so it lines up
// with the plain-IPv4 form the same client would produce over a v4 path —
// otherwise the two forms would occupy separate rate-limit buckets for what
// is the same address.
func TestClientIPv4MappedXRealIP(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", "::ffff:203.0.113.9")

	got := clientIP(r, defaultProxies)
	want := "203.0.113.9"
	if got != want {
		t.Errorf("clientIP() = %q, want %q (X-Real-IP in IPv4-mapped form should be unmapped)", got, want)
	}
}

// TestClientIPEmptyXFFHeaderFallsThroughToXRealIP covers a header that is
// present but empty (as opposed to absent) — a subtly different case than
// "not set" that a naive `r.Header.Get(...) != ""` check handles correctly,
// but is worth pinning down explicitly since it's easy to regress.
func TestClientIPEmptyXFFHeaderFallsThroughToXRealIP(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "") // present, empty
	r.Header.Set("X-Real-IP", "203.0.113.9")

	if got, want := clientIP(r, defaultProxies), "203.0.113.9"; got != want {
		t.Errorf("clientIP() = %q, want %q", got, want)
	}
}

// TestClientIPXFFWithEmptyEntries covers stray commas (e.g. "a,,b" or a
// trailing comma), which must be skipped rather than crash or be treated as
// a valid (empty) hop.
func TestClientIPXFFWithEmptyEntries(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "203.0.113.7,,198.51.100.9,")

	if got, want := clientIP(r, defaultProxies), "198.51.100.9"; got != want {
		t.Errorf("clientIP() = %q, want %q", got, want)
	}
}

// TestClientIPInvalidXRealIPFromTrustedProxy verifies that an invalid
// X-Real-IP header from a trusted proxy is rejected, falling back to RemoteAddr
// instead of blindly returning the invalid value.
func TestClientIPInvalidXRealIPFromTrustedProxy(t *testing.T) {
	defaultProxies := mustPrefixes(t, "127.0.0.1/32", "::1/128")

	r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Real-IP", "not-a-valid-ip-address")

	if got, want := clientIP(r, defaultProxies), "127.0.0.1"; got != want {
		t.Errorf("clientIP() = %q, want %q (invalid X-Real-IP should fall back to RemoteAddr)", got, want)
	}
}

// TestRateLimiterCleanup pins down the cleanup contract directly (rather
// than via the 5-minute ticker in cleanupLimiters), since the refactor into
// a reusable rateLimiter type is exactly the kind of change that could
// silently break per-instance cleanup or leak entries across instances.
func TestRateLimiterCleanup(t *testing.T) {
	rl := newRateLimiter(60, 1)
	rl.allow("1.1.1.1")
	rl.allow("2.2.2.2")

	v, ok := rl.entries.Load("1.1.1.1")
	if !ok {
		t.Fatal("expected an entry for 1.1.1.1 after allow()")
	}
	v.(*limitEntry).lastSeen.Store(time.Now().Add(-1 * time.Hour).UnixNano())

	rl.cleanup(time.Now().Add(-expireAfter).UnixNano())

	if _, ok := rl.entries.Load("1.1.1.1"); ok {
		t.Error("stale entry for 1.1.1.1 should have been removed by cleanup")
	}
	if _, ok := rl.entries.Load("2.2.2.2"); !ok {
		t.Error("fresh entry for 2.2.2.2 should have survived cleanup")
	}
}

// TestRateLimitersAreIndependent guards the core promise of the refactor:
// askLimiter and mcpLimiter must not share a budget for the same IP.
func TestRateLimitersAreIndependent(t *testing.T) {
	ask := newRateLimiter(ratePerMinute, rateBurst)
	mcp := newRateLimiter(mcpRatePerMinute, mcpRateBurst)
	ip := "203.0.113.50"

	for i := 0; i < rateBurst; i++ {
		if !ask.allow(ip) {
			t.Fatalf("ask limiter: request %d should be within burst", i)
		}
	}
	if ask.allow(ip) {
		t.Fatal("ask limiter should be exhausted after burst requests")
	}
	if !mcp.allow(ip) {
		t.Error("mcp limiter should not share its budget with the ask limiter for the same IP")
	}
}

// TestAskAndFeedbackShareRateLimitBudget pins down that /api/ask and
// /api/feedback still share one budget per IP after the refactor (they are
// both wired to s.askLimiter in New()) — this is pre-existing behaviour, not
// a regression, but it is exactly the kind of thing a limiter refactor could
// accidentally change (e.g. by giving each route its own rateLimiter
// instance instead of sharing one).
func TestAskAndFeedbackShareRateLimitBudget(t *testing.T) {
	s := &srv{askLimiter: newRateLimiter(ratePerMinute, rateBurst)}
	next := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }
	askHandler := s.withRateLimit(s.askLimiter, next)
	feedbackHandler := s.withRateLimit(s.askLimiter, next)

	ip := "203.0.113.60:1234"
	for i := 0; i < rateBurst; i++ {
		r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		askHandler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("ask request %d: status = %d, want 200", i, w.Code)
		}
	}

	// The burst is now exhausted by /api/ask alone. A /api/feedback request
	// from the same IP must be rejected too, because both routes draw from
	// the same per-IP bucket.
	r := httptest.NewRequest(http.MethodPost, "/api/feedback", nil)
	r.RemoteAddr = ip
	w := httptest.NewRecorder()
	feedbackHandler(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("feedback after exhausting ask's budget: status = %d, want 429", w.Code)
	}
}

// TestNewInvalidTrustedProxies verifies New() fails startup on a malformed
// TRUSTED_PROXIES value instead of silently falling back to "trust nothing"
// or "trust everything".
func TestNewInvalidTrustedProxies(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "not-a-cidr")
	t.Setenv("MCP_TOKEN", "")

	handler, err := New(nil, embedder.New(), llm.New())
	if err == nil {
		t.Fatal("New() with an invalid TRUSTED_PROXIES should return an error")
	}
	if handler != nil {
		t.Error("New() should return a nil handler alongside the error")
	}
}

// TestMCPRouteMountingIsExact checks that only the exact "/api/mcp" path
// with one of POST/GET/DELETE reaches the (auth + rate-limited) MCP
// pipeline. None of these near-miss requests should be routed there — they
// must be rejected by the mux itself (405/404) without ever reaching the
// handler, so they can't be used to bypass MCP_TOKEN or the MCP rate
// limiter via a path or method the builder didn't intend to expose.
func TestMCPRouteMountingIsExact(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "s3cr3t")

	handler, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
	}{
		{"PUT is not a registered MCP method", http.MethodPut, "/api/mcp"},
		{"OPTIONS is not a registered MCP method", http.MethodOptions, "/api/mcp"},
		{"trailing slash does not match the exact pattern", http.MethodPost, "/api/mcp/"},
		{"a sub-path does not match the exact pattern", http.MethodPost, "/api/mcp/extra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)

			// Neither of these is the MCP handler's own 401 (unauthorized,
			// which would mean the request *did* reach the pipeline) or 200.
			if w.Code == http.StatusUnauthorized || w.Code == http.StatusOK {
				t.Errorf("%s %s: status = %d, this request should never reach the MCP handler", tc.method, tc.path, w.Code)
			}
			if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 405 or 404", tc.method, tc.path, w.Code)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{name: "empty string uses the default", raw: "", wantLen: 2},
		{name: "single CIDR", raw: "10.0.0.0/8", wantLen: 1},
		{name: "multiple CIDRs with spaces", raw: "10.0.0.0/8, 192.168.0.0/16 , ::1/128", wantLen: 3},
		{name: "blank entries between commas are skipped", raw: "127.0.0.1/32,,::1/128", wantLen: 2},
		{name: "invalid CIDR is a startup error", raw: "not-a-cidr", wantErr: true},
		{name: "one invalid entry among valid ones is still an error", raw: "127.0.0.1/32,garbage", wantErr: true},
		{name: "bare IP without a prefix length is invalid", raw: "127.0.0.1", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTrustedProxies(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseTrustedProxies(%q) = %v, want error", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTrustedProxies(%q) unexpected error: %v", tc.raw, err)
			}
			if len(got) != tc.wantLen {
				t.Errorf("parseTrustedProxies(%q) = %d prefixes, want %d", tc.raw, len(got), tc.wantLen)
			}
		})
	}
}
