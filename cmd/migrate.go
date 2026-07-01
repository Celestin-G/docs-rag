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
  content_tsv TSVECTOR,
  badge       TEXT,
  version_num SMALLINT,
  deployment  TEXT NOT NULL DEFAULT 'both',
  created_at  TIMESTAMPTZ DEFAULT NOW()
);

CREATE INDEX doc_chunks_embedding_idx
  ON doc_chunks USING hnsw (embedding vector_cosine_ops);

CREATE INDEX doc_chunks_tsv_idx
  ON doc_chunks USING GIN(content_tsv);

-- Feedback and analytics tables survive re-migration (IF NOT EXISTS).
CREATE TABLE IF NOT EXISTS feedback (
  id         SERIAL PRIMARY KEY,
  question   TEXT NOT NULL,
  answer     TEXT NOT NULL,
  rating     SMALLINT NOT NULL,
  created_at TIMESTAMPTZ DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS request_logs (
  id           SERIAL PRIMARY KEY,
  question     TEXT NOT NULL,
  duration_ms  INTEGER NOT NULL,
  chunks_found INTEGER NOT NULL,
  status       TEXT NOT NULL,
  created_at   TIMESTAMPTZ DEFAULT NOW()
);
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
