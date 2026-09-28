package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "rag",
	Short: "RAG CLI for Formance documentation",
}

// Execute runs the root command with a context that is cancelled on SIGINT
// or SIGTERM. Every RunE reads it via cmd.Context() rather than
// context.Background(), so Ctrl-C (or a container orchestrator's SIGTERM)
// propagates as cancellation everywhere: serve gets the signal to start a
// graceful shutdown, and ingest/ask/migrate stop at their next context check
// instead of running to completion regardless.
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fmt.Fprintf(os.Stderr, "error: %s environment variable is required\n", key)
		os.Exit(1)
	}
	return v
}
