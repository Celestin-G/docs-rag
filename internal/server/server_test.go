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
	mappedHopProxies := mustPrefixes(t, "127.0.0.1/32", "10.0.0.9/32")

	for _, tc := range []struct {
		name       string
		remoteAddr string
		xff        string
		xri        string
		proxies    []netip.Prefix
		want       string
	}{
		{name: "trusted proxy, single-hop XFF returns the address the proxy actually saw", remoteAddr: "127.0.0.1:5555", xff: "9.9.9.9, 203.0.113.7", proxies: defaultProxies, want: "203.0.113.7"},
		{name: "trusted proxy, spoofed leftmost XFF entry is ignored", remoteAddr: "127.0.0.1:5555", xff: "203.0.113.7, 198.51.100.9", proxies: defaultProxies, want: "198.51.100.9"},
		{name: "trusted proxy, multi-hop XFF skips trusted hops", remoteAddr: "127.0.0.1:5555", xff: "203.0.113.7, 10.0.0.9", proxies: mappedHopProxies, want: "203.0.113.7"},
		{name: "trusted proxy, IPv4-mapped trusted hop in XFF is still skipped", remoteAddr: "127.0.0.1:5555", xff: "203.0.113.7, ::ffff:10.0.0.9", proxies: mappedHopProxies, want: "203.0.113.7"},
		{name: "untrusted RemoteAddr: XFF header ignored", remoteAddr: "8.8.8.8:1234", xff: "203.0.113.7", proxies: defaultProxies, want: "8.8.8.8"},
		{name: "untrusted RemoteAddr: X-Real-IP header ignored", remoteAddr: "8.8.8.8:1234", xri: "203.0.113.7", proxies: defaultProxies, want: "8.8.8.8"},
		{name: "trusted proxy, X-Real-IP used when no XFF", remoteAddr: "127.0.0.1:1234", xri: "203.0.113.9", proxies: defaultProxies, want: "203.0.113.9"},
		{name: "trusted proxy, IPv4-mapped X-Real-IP is unmapped", remoteAddr: "127.0.0.1:1234", xri: "::ffff:203.0.113.9", proxies: defaultProxies, want: "203.0.113.9"},
		{name: "trusted proxy, missing headers falls back to RemoteAddr", remoteAddr: "127.0.0.1:1234", proxies: defaultProxies, want: "127.0.0.1"},
		{name: "RemoteAddr without a port", remoteAddr: "127.0.0.1", proxies: defaultProxies, want: "127.0.0.1"},
		{name: "malformed XFF entries are skipped", remoteAddr: "127.0.0.1:1234", xff: "not-an-ip, 203.0.113.7", proxies: defaultProxies, want: "203.0.113.7"},
		{name: "XFF made only of trusted proxy addresses falls back to RemoteAddr", remoteAddr: "127.0.0.1:1234", xff: "10.0.0.9", proxies: mappedHopProxies, want: "127.0.0.1"},
		{name: "IPv4-mapped RemoteAddr is recognised as its plain-IPv4 trusted proxy", remoteAddr: "[::ffff:127.0.0.1]:5555", xff: "203.0.113.7", proxies: defaultProxies, want: "203.0.113.7"},
		{name: "invalid X-Real-IP from a trusted proxy falls back to RemoteAddr", remoteAddr: "127.0.0.1:1234", xri: "not-a-valid-ip-address", proxies: defaultProxies, want: "127.0.0.1"},
		{name: "XFF with empty entries between commas", remoteAddr: "127.0.0.1:1234", xff: "203.0.113.7,,198.51.100.9,", proxies: defaultProxies, want: "198.51.100.9"},
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

// TestRateLimiterCleanup: entries not seen since cutoff are evicted, fresher
// ones survive.
func TestRateLimiterCleanup(t *testing.T) {
	rl := newRateLimiter(ratePerMinute, rateBurst)
	rl.allow("stale")
	rl.allow("fresh")
	if v, ok := rl.entries.Load("stale"); ok {
		v.(*limitEntry).lastSeen.Store(time.Now().Add(-time.Hour).UnixNano())
	}

	rl.cleanup(time.Now().Add(-time.Minute).UnixNano())

	if _, ok := rl.entries.Load("stale"); ok {
		t.Error("stale entry not evicted")
	}
	if _, ok := rl.entries.Load("fresh"); !ok {
		t.Error("fresh entry evicted")
	}
}

// TestNewRateLimitWiring exercises the actual limiter wiring done by New():
// GET /api/mcp requires auth and is rate-limited on its own budget
// (mcpLimiter), while /api/ask and /api/feedback share one budget
// (askLimiter) that MCP does not draw from.
func TestNewRateLimitWiring(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "tok")
	h, err := New(nil, embedder.New(), llm.New())
	if err != nil {
		t.Fatalf("New(): %v", err)
	}

	t.Run("GET /api/mcp: no token is 401, then wrong-token retries hit 429", func(t *testing.T) {
		ip := "203.0.113.80:1"
		r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("no-token GET status = %d, want 401", w.Code)
		}

		var last int
		for i := 0; i < mcpRateBurst; i++ {
			r := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
			r.RemoteAddr = ip
			r.Header.Set("Authorization", "Bearer wrong")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			last = w.Code
		}
		if last != http.StatusTooManyRequests {
			t.Errorf("wrong-token GET after burst status = %d, want 429", last)
		}
	})

	t.Run("exhausting /api/ask rate-limits /api/feedback but not MCP", func(t *testing.T) {
		ip := "203.0.113.81:1"
		for i := 0; i < rateBurst; i++ {
			r := httptest.NewRequest(http.MethodPost, "/api/ask", nil)
			r.RemoteAddr = ip
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
		}

		r := httptest.NewRequest(http.MethodPost, "/api/feedback", nil)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusTooManyRequests {
			t.Errorf("feedback status = %d, want 429 (shares ask's budget)", w.Code)
		}

		r = httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
		r.RemoteAddr = ip
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Error("MCP should not share the ask/feedback rate budget")
		}
	})
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
