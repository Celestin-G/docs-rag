package cmd

import (
	"context"
	"fmt"

	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

// migrationSQL brings the schema up to date without destroying anything.
//
// It used to DROP and recreate doc_chunks, which was convenient while the
// columns were still moving but made the migration impossible to run before a
// release: dropping the table empties the index, so the schema could only be
// updated during the nightly ingest, and a server deployed before that ran
// queried columns that did not exist yet. Every column added so far is purely
// additive, so ADD COLUMN IF NOT EXISTS reaches the same schema while the
// existing rows keep serving. New columns read as unconstrained on old rows —
// visibility NULL is an ungated page, version_below NULL an open upper bound —
// so the gating is merely as coarse as before until the next ingest, never wrong
// in a way that hides documentation.
//
// Being idempotent is what lets this run as an init container on the server
// itself, which is the only way to guarantee the schema is in place before the
// first request rather than hoping an out-of-band job ran first.
const migrationSQL = `
CREATE EXTENSION IF NOT EXISTS vector;

-- Serialise concurrent migrations: replicas start together, and two sessions
-- both finding a column missing would otherwise race to add it. Transaction
-- scoped, so it is released whatever happens below.
SELECT pg_advisory_xact_lock(hashtext('docs_rag_migrate'));

-- The table body itself lives in internal/store, next to the staging table a
-- re-ingest swaps in, so the two cannot drift apart.
%s

-- Columns added after the table first shipped. The CREATE above already lists
-- them for a fresh database; these bring an existing one to the same shape.
-- Keep the two in step: a column added in one and not the other means the
-- schema depends on whether the database is new, which is the worst kind of
-- difference to debug.
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS badge             TEXT;
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS version_num       SMALLINT;
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS version_below     SMALLINT;
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS version_component TEXT NOT NULL DEFAULT 'stack';
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS visibility        JSONB;
ALTER TABLE doc_chunks ADD COLUMN IF NOT EXISTS deployment        TEXT NOT NULL DEFAULT 'both';

CREATE INDEX IF NOT EXISTS doc_chunks_embedding_idx
  ON doc_chunks USING hnsw (embedding vector_cosine_ops);

CREATE INDEX IF NOT EXISTS doc_chunks_tsv_idx
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

	if err := st.Exec(ctx, fmt.Sprintf(migrationSQL, store.ChunkTableDDL("doc_chunks", true))); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}

	fmt.Println("Migration applied successfully.")
	return nil
}
