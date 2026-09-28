package server

import (
	"testing"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
)

// TestEnvDuration covers the fallback contract shared by every
// ANSWER_*_TIMEOUT / ANSWER_TOTAL_BUDGET override: unset or malformed falls
// back to the caller's default instead of panicking or producing a zero
// (immediately-expired) deadline.
func TestEnvDuration(t *testing.T) {
	const key = "TEST_ENV_DURATION"
	const def = 7 * time.Second

	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses the default", env: "", want: def},
		{name: "valid duration overrides the default", env: "250ms", want: 250 * time.Millisecond},
		{name: "malformed value falls back to the default", env: "not-a-duration", want: def},
		{name: "unitless number falls back to the default", env: "5", want: def},
		{name: "negative duration falls back to the default", env: "-1s", want: def},
		{name: "zero falls back to the default", env: "0s", want: def},
		{name: "unitless zero falls back to the default", env: "0", want: def},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(key, tc.env)
			if got := envDuration(key, def); got != tc.want {
				t.Errorf("envDuration(%q, %s) = %s, want %s", key, def, got, tc.want)
			}
		})
	}
}

// TestNewStageTimeoutDefaultsAndOverrides pins down that New() actually
// wires the ANSWER_*_TIMEOUT / ANSWER_TOTAL_BUDGET env vars into the
// per-stage fields answer() reads, both when unset (defaults apply) and
// when set (the override is used) — the two halves of envDuration's
// contract, exercised through the constructor that actually calls it.
func TestNewStageTimeoutDefaultsAndOverrides(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "")
	t.Setenv("MCP_TOKEN", "")

	t.Run("defaults apply when unset", func(t *testing.T) {
		h, err := New(nil, embedder.New(), llm.New())
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		for _, tc := range []struct {
			name string
			got  time.Duration
			want time.Duration
		}{
			{"translateTimeout", h.s.translateTimeout, defaultTranslateTimeout},
			{"embedTimeout", h.s.embedTimeout, defaultEmbedTimeout},
			{"searchTimeout", h.s.searchTimeout, defaultSearchTimeout},
			{"totalAnswerBudget", h.s.totalAnswerBudget, defaultTotalAnswerBudget},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %s, want default %s", tc.name, tc.got, tc.want)
			}
		}
	})

	t.Run("env overrides apply when set", func(t *testing.T) {
		t.Setenv("ANSWER_TRANSLATE_TIMEOUT", "1s")
		t.Setenv("ANSWER_EMBED_TIMEOUT", "2s")
		t.Setenv("ANSWER_SEARCH_TIMEOUT", "3s")
		t.Setenv("ANSWER_TOTAL_BUDGET", "4s")

		h, err := New(nil, embedder.New(), llm.New())
		if err != nil {
			t.Fatalf("New(): %v", err)
		}
		for _, tc := range []struct {
			name string
			got  time.Duration
			want time.Duration
		}{
			{"translateTimeout", h.s.translateTimeout, time.Second},
			{"embedTimeout", h.s.embedTimeout, 2 * time.Second},
			{"searchTimeout", h.s.searchTimeout, 3 * time.Second},
			{"totalAnswerBudget", h.s.totalAnswerBudget, 4 * time.Second},
		} {
			if tc.got != tc.want {
				t.Errorf("%s = %s, want override %s", tc.name, tc.got, tc.want)
			}
		}
	})
}
