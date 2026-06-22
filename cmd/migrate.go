package cmd

import (
	"context"
	"fmt"

	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

// migrationSQL drops and recreates the table to handle dimension changes across runs.
const migrationSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

DROP TABLE IF EXISTS doc_chunks;

CREATE TABLE doc_chunks (
  id          SERIAL PRIMARY KEY,
  source_file TEXT NOT NULL,
  section     TEXT,
  content     TEXT NOT NULL,
  embedding   vector(768),
  created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX doc_chunks_embedding_idx
  ON doc_chunks USING hnsw (embedding vector_cosine_ops);
`

var migrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Apply database migrations (creates extension and table)",
	RunE:  runMigrate,
}

func init() {
	rootCmd.AddCommand(migrateCmd)
}

func runMigrate(_ *cobra.Command, _ []string) error {
	ctx := context.Background()
	dbURL := mustEnv("DATABASE_URL")

	st, err := store.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	if err := st.Exec(ctx, migrationSQL); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}

	fmt.Println("Migration applied successfully.")
	return nil
}
