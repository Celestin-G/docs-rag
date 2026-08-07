package store

import (
	"context"
	"os"
	"testing"
)

// The point of staging is that the live index never stops answering. This walks
// the sequence a re-ingest follows and checks the live table at every step.
func TestStagingSwap(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping the SQL-level check")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()

	if err := s.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector; DROP TABLE IF EXISTS "+chunksTable+" CASCADE;"+ChunkTableDDL(chunksTable, false)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	live := func() int64 {
		var n int64
		if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+chunksTable).Scan(&n); err != nil {
			t.Fatalf("count live: %v", err)
		}
		return n
	}
	insert := func(file string) {
		t.Helper()
		if err := s.Insert(ctx, DocChunk{SourceFile: file, Section: "s", Content: "c", Embedding: make([]float32, 768)},
			"", 0, 0, "stack", "both", nil); err != nil {
			t.Fatalf("insert %s: %v", file, err)
		}
	}

	insert("old-1.mdx")
	insert("old-2.mdx")
	if live() != 2 {
		t.Fatalf("setup: live has %d rows, want 2", live())
	}

	// A re-ingest starts. The live index must keep every row.
	if err := s.StageCreate(ctx); err != nil {
		t.Fatalf("stage create: %v", err)
	}
	if live() != 2 {
		t.Errorf("live index lost rows when staging began: %d, want 2", live())
	}
	insert("new-1.mdx")
	if live() != 2 {
		t.Errorf("writes leaked into the live index mid-run: %d, want 2", live())
	}

	if err := s.StagePromote(ctx); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if live() != 1 {
		t.Errorf("after the swap live has %d rows, want the 1 staged", live())
	}
	var file string
	if err := s.pool.QueryRow(ctx, "SELECT source_file FROM "+chunksTable).Scan(&file); err != nil {
		t.Fatalf("read promoted row: %v", err)
	}
	if file != "new-1.mdx" {
		t.Errorf("promoted row is %q, want the staged one", file)
	}

	// Writes must go back to the live table once the swap is done.
	insert("after.mdx")
	if live() != 2 {
		t.Errorf("post-swap insert did not land in the live table: %d rows, want 2", live())
	}

	// And a second run must not collide with the names the first one renamed.
	if err := s.StageCreate(ctx); err != nil {
		t.Fatalf("second stage create: %v", err)
	}
	insert("second-run.mdx")
	if err := s.StagePromote(ctx); err != nil {
		t.Fatalf("second promote: %v", err)
	}
	if live() != 1 {
		t.Errorf("second swap left %d rows, want 1", live())
	}
}

// Promoting nothing would recreate the outage staging exists to prevent.
func TestStagingRefusesToPromoteAnEmptyIndex(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping the SQL-level check")
	}
	ctx := context.Background()
	s, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer s.Close()

	if err := s.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector; DROP TABLE IF EXISTS "+chunksTable+" CASCADE;"+ChunkTableDDL(chunksTable, false)); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := s.Insert(ctx, DocChunk{SourceFile: "kept.mdx", Content: "c", Embedding: make([]float32, 768)},
		"", 0, 0, "stack", "both", nil); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.StageCreate(ctx); err != nil {
		t.Fatalf("stage create: %v", err)
	}
	if err := s.StagePromote(ctx); err == nil {
		t.Fatal("promoted an empty staging table over a populated index")
	}
	var n int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM "+chunksTable).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("live index has %d rows after the refusal, want the original 1", n)
	}
	if err := s.StageDiscard(ctx); err != nil {
		t.Fatalf("discard: %v", err)
	}
}
