package cmd

import (
	"testing"
)

// SHUTDOWN_TIMEOUT=0s or a negative value makes Shutdown return immediately,
// skipping the drain entirely. The existing test's doc comment promises to
// guard against "silently producing a zero (immediate-kill) deadline", but
// only non-parseable values are rejected.
func TestShutdownTimeoutRejectsNonPositive(t *testing.T) {
	for _, v := range []string{"0", "0s", "-5s"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SHUTDOWN_TIMEOUT", v)
			if got := shutdownTimeout(); got != defaultShutdownTimeout {
				t.Errorf("shutdownTimeout() with %q = %s, want default %s", v, got, defaultShutdownTimeout)
			}
		})
	}
}
