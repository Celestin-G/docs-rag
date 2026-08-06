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

CREATE TABLE IF NOT EXISTS doc_chunks (
  id          SERIAL PRIMARY KEY,
  source_file TEXT NOT NULL,
  section     TEXT,
  content     TEXT NOT NULL,
  embedding   vector(768),
  content_tsv TSVECTOR,
  badge       TEXT,
  -- version_num is the lower bound: the stack version this content appears in.
  -- version_below is the exclusive upper bound, NULL when the content still
  -- applies to the latest version. The pair mirrors the docs framework's
  -- version-composition ranges ({min: "v3.0", below: "v3.1"}), which a single
  -- lower bound cannot express — without it, v3.0 API operations surface for a
  -- question scoped to v3.2.
  version_num   SMALLINT,
  version_below SMALLINT,
  -- Which component the version bounds are expressed against. Pages declare it
  -- in frontmatter as "component: numscript-interpreter" and default to the
  -- stack. Without it a ledger "2.3" constraint reads as stack v2.3 and a
  -- "0.0.15" interpreter constraint collapses to zero, so both stop filtering
  -- anything. Only stack-scoped bounds are compared against the stack version
  -- the user selects; the rest are stored for the per-component cascade, which
  -- needs the framework's composition graph to be machine-readable first.
  version_component TEXT NOT NULL DEFAULT 'stack',
  -- Which dimension values the page applies to, e.g. {"numscript": ["new"]}.
  -- This is how the docs hide experimental features: a page carrying
  -- visibility.numscript = new does not exist when the Legacy Machine is
  -- selected. Distinct from the version bounds above — a page can be gated on a
  -- dimension without any version constraint, and the reverse.
  visibility JSONB,
  deployment  TEXT NOT NULL DEFAULT 'both',
  created_at  TIMESTAMPTZ DEFAULT NOW()
);

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

	if err := st.Exec(ctx, migrationSQL); err != nil {
		return fmt.Errorf("apply migration: %w", err)
	}

	fmt.Println("Migration applied successfully.")
	return nil
}
