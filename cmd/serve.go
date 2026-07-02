package cmd

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/server"
	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the web UI on http://localhost:8080",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().String("addr", ":8080", "Address to listen on")
	rootCmd.AddCommand(serveCmd)
}

func runServe(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()
	dbURL := mustEnv("DATABASE_URL")
	addr, _ := cmd.Flags().GetString("addr")

	st, err := store.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	handler := server.New(st, embedder.New(), llm.New())

	srv := &http.Server{
		Addr:    addr,
		Handler: handler,
		// No WriteTimeout: SSE responses legitimately stream for 60s+.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("Formance Docs RAG — http://localhost%s\n", addr)
	return srv.ListenAndServe()
}
