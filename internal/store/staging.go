package store

import (
	"context"
	"fmt"
)

// A full re-ingest used to TRUNCATE and refill in place, which meant the index
// was empty for the whole run — around half an hour — and stayed empty until
// the next attempt if anything failed partway. That is exactly how the index
// went missing for a day: an eviction mid-run, and nothing to notice it because
// an empty index answers "no result" rather than erroring.
//
// So a re-ingest now fills a staging table and swaps it in at the end. The live
// table keeps serving every query until the last second, and a run that dies
// leaves it untouched. The swap itself is one transaction: readers block on the
// lock for the moment it takes to rename, then see the complete new index.
const (
	chunksTable  = "doc_chunks"
	stagingTable = "doc_chunks_staging"
)

// chunkTableBody is the shape of the chunk table, written once so the live
// table and the staging table cannot drift apart. migrate.go creates the live
// one from it; StageCreate creates the staging one.
const chunkTableBody = `(
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
)`

// ChunkTableDDL returns the statement creating the chunk table under a given
// name. Exported so the migration builds the live table from the same source as
// the staging one.
func ChunkTableDDL(name string, ifNotExists bool) string {
	exists := ""
	if ifNotExists {
		exists = "IF NOT EXISTS "
	}
	return "CREATE TABLE " + exists + name + " " + chunkTableBody + ";"
}

// StageCreate drops any staging table left behind by a failed run and creates a
// fresh one. Indexes are deliberately not created yet: building the HNSW index
// as rows arrive costs far more than building it once at the end.
func (s *Store) StageCreate(ctx context.Context) error {
	sql := "DROP TABLE IF EXISTS " + stagingTable + ";\n" + ChunkTableDDL(stagingTable, false)
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("create staging table: %w", err)
	}
	s.target = stagingTable
	return nil
}

// StagePromote builds the indexes on the staging table and swaps it into place.
//
// The swap is one transaction, so a reader either sees the whole old index or
// the whole new one. The sequence and indexes are renamed alongside the table:
// without that, the next run would collide with names still held by the table
// it just promoted.
func (s *Store) StagePromote(ctx context.Context) error {
	index := `
CREATE INDEX ` + stagingTable + `_embedding_idx ON ` + stagingTable + ` USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ` + stagingTable + `_tsv_idx ON ` + stagingTable + ` USING GIN(content_tsv);`
	if _, err := s.pool.Exec(ctx, index); err != nil {
		return fmt.Errorf("index staging table: %w", err)
	}

	// Refuse to promote an empty staging table. Swapping one in would reproduce
	// the outage this whole mechanism exists to prevent, and the only way to be
	// here with zero rows is a bug upstream.
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+stagingTable).Scan(&n); err != nil {
		return fmt.Errorf("count staging rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("staging table is empty — refusing to promote it over a live index")
	}

	swap := `
DROP TABLE IF EXISTS ` + chunksTable + `;
ALTER TABLE ` + stagingTable + ` RENAME TO ` + chunksTable + `;
ALTER SEQUENCE ` + stagingTable + `_id_seq RENAME TO ` + chunksTable + `_id_seq;
ALTER INDEX ` + stagingTable + `_embedding_idx RENAME TO ` + chunksTable + `_embedding_idx;
ALTER INDEX ` + stagingTable + `_tsv_idx RENAME TO ` + chunksTable + `_tsv_idx;`
	if _, err := s.pool.Exec(ctx, swap); err != nil {
		return fmt.Errorf("promote staging table: %w", err)
	}
	s.target = chunksTable
	return nil
}

// StageDiscard drops the staging table after a failed run, so the next attempt
// does not inherit a half-filled one. Errors are the caller's to ignore: the
// run has already failed, and StageCreate drops it again anyway.
func (s *Store) StageDiscard(ctx context.Context) error {
	s.target = chunksTable
	_, err := s.pool.Exec(ctx, "DROP TABLE IF EXISTS "+stagingTable)
	return err
}
