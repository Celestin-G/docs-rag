package cmd

import (
	"testing"
	"time"
)

// TestShutdownTimeout covers SHUTDOWN_TIMEOUT's env-parsing contract: unset,
// malformed, or non-positive falls back to defaultShutdownTimeout rather than
// panicking or silently producing a zero (immediate-kill) deadline.
func TestShutdownTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset uses the default", env: "", want: defaultShutdownTimeout},
		{name: "valid duration overrides the default", env: "45s", want: 45 * time.Second},
		{name: "valid duration with minutes", env: "2m", want: 2 * time.Minute},
		{name: "malformed value falls back to the default", env: "not-a-duration", want: defaultShutdownTimeout},
		{name: "unitless number falls back to the default", env: "30", want: defaultShutdownTimeout},
		{name: "zero falls back to the default", env: "0", want: defaultShutdownTimeout},
		{name: "zero with unit falls back to the default", env: "0s", want: defaultShutdownTimeout},
		{name: "negative falls back to the default", env: "-5s", want: defaultShutdownTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SHUTDOWN_TIMEOUT", tc.env)
			if got := shutdownTimeout(); got != tc.want {
				t.Errorf("shutdownTimeout() = %s, want %s", got, tc.want)
			}
		})
	}
}
