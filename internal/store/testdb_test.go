package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
)

// testDSN returns TEST_DATABASE_URL, skipping the test when it is unset.
//
// Store tests must never read DATABASE_URL. The README has developers export
// that in their shell for everyday use, and staging_test.go in particular
// used to run DROP TABLE ... CASCADE against whatever database it was given
// — reading DATABASE_URL here turned a plain `go test ./...` into a way to
// wipe the real dev index. TEST_DATABASE_URL is a separate, deliberate
// opt-in; see the test-db service in docker-compose.yml.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping the SQL-level check (start docker-compose.yml's test-db service and export TEST_DATABASE_URL to run it)")
	}
	return dsn
}

// testStore connects to TEST_DATABASE_URL, creates a throwaway schema with a
// random name and points every connection the returned Store's pool opens at
// it via search_path, then creates the chunks table inside that schema. The
// schema — and everything created in it during the test — is dropped in
// t.Cleanup, so callers never need their own reset/teardown SQL.
//
// Isolation relies on store.go and staging.go never schema-qualifying a
// table name (always "doc_chunks", never "public.doc_chunks" — verified by
// reading both files): putting the throwaway schema first on search_path is
// therefore enough to keep every statement the Store issues confined to it.
// public stays second on the path so the pgvector "vector" type — an
// extension type, necessarily database-wide rather than per-schema — still
// resolves. Nothing in this package other than the bootstrap step below ever
// touches public, so a pre-existing public.doc_chunks row is never read,
// written, or dropped by these tests.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()

	schema := randomSchemaName(t)

	// Bootstrap on the plain DSN (default search_path, i.e. public): the
	// extension belongs in public regardless of which schema the test
	// itself targets, and the throwaway schema doesn't exist yet for
	// anything to have pointed at it.
	boot, err := New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect (bootstrap): %v", err)
	}
	if err := boot.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS vector"); err != nil {
		boot.Close()
		t.Fatalf("create vector extension: %v", err)
	}
	if err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	boot.Close()

	t.Cleanup(func() {
		cleanup, err := New(context.Background(), dsn)
		if err != nil {
			t.Errorf("connect (cleanup) for schema %s: %v", schema, err)
			return
		}
		defer cleanup.Close()
		if err := cleanup.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	schemaedDSN, err := withSearchPath(t, dsn, schema+",public")
	if err != nil {
		t.Fatalf("withSearchPath: %v", err)
	}
	s, err := New(ctx, schemaedDSN)
	if err != nil {
		t.Fatalf("connect (schema %s): %v", schema, err)
	}
	t.Cleanup(s.Close)

	if err := s.Exec(ctx, ChunkTableDDL(chunksTable, false)); err != nil {
		t.Fatalf("create %s in schema %s: %v", chunksTable, schema, err)
	}

	return s
}

// randomSchemaName generates a name of the form rag_test_<16 hex chars>:
// short enough to stay well under Postgres's 63-byte identifier limit, and
// restricted to [a-f0-9] so it is always a safe unquoted identifier to
// interpolate into CREATE/DROP SCHEMA statements.
func randomSchemaName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate random schema name: %v", err)
	}
	return "rag_test_" + hex.EncodeToString(b[:])
}

// withSearchPath returns dsn with its search_path query parameter set to
// path. pgconn.ParseConfig passes any connection-string parameter it doesn't
// specifically recognise straight into RuntimeParams, sent as a session
// default when the connection starts up — search_path isn't one of libpq's
// special keys, so this is enough to pin every connection pgxpool opens to
// path without a per-connection AfterConnect hook.
//
// Only the postgres:// or postgresql:// URL form is supported; libpq
// keyword/value DSNs (e.g. "host=localhost user=rag ...") are rejected with an error.
func withSearchPath(t *testing.T, dsn, path string) (string, error) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return "", fmt.Errorf("TEST_DATABASE_URL must be a postgres:// or postgresql:// URL (got scheme %q)", u.Scheme)
	}
	q := u.Query()
	q.Set("search_path", path)
	u.RawQuery = q.Encode()
	return u.String(), nil
}
