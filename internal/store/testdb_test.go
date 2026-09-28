package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// testDSN returns TEST_DATABASE_URL, skipping the test when it is unset.
// Never read DATABASE_URL here — that would turn `go test ./...` into a way
// to wipe a developer's real dev index.
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping the SQL-level check (start docker-compose.yml's test-db service and export TEST_DATABASE_URL to run it)")
	}
	return dsn
}

// testStore connects to TEST_DATABASE_URL, creates a throwaway schema with a
// random name, points every connection the returned Store's pool opens at it
// via search_path, then creates the chunks table inside it. Everything
// created is dropped in t.Cleanup. Isolation relies on store.go/staging.go
// never schema-qualifying a table name; public stays second on the path so
// pgvector's database-wide "vector" type still resolves.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := testDSN(t)
	ctx := context.Background()

	schema := randomSchemaName(t)

	// Bootstrap on the plain DSN (default search_path): the extension
	// belongs in public regardless of the test's schema, which doesn't
	// exist yet for anything to have pointed at it.
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

// schemaNameRE is the contract randomSchemaName promises: short enough for
// Postgres's 63-byte identifier limit, and restricted to [a-f0-9] so it is
// always safe to interpolate unquoted into CREATE/DROP SCHEMA.
var schemaNameRE = regexp.MustCompile(`^rag_test_[0-9a-f]{16}$`)

// randomSchemaName generates a name matching schemaNameRE.
func randomSchemaName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("generate random schema name: %v", err)
	}
	return "rag_test_" + hex.EncodeToString(b[:])
}

// withSearchPath sets dsn's search_path query parameter to path, so every
// connection pgxpool opens gets it as a session default. Only postgres:// /
// postgresql:// URLs are supported; libpq keyword/value DSNs error out.
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

// TestWithSearchPath covers withSearchPath's DSN-rewriting contract; it is a
// pure function, so this runs even without TEST_DATABASE_URL.
func TestWithSearchPath(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		wantErr bool
		check   func(t *testing.T, out string)
	}{
		{
			name: "preserves scheme, host, path and an existing query param",
			in:   "postgres://rag:rag@localhost:5432/ragdb?sslmode=disable",
			check: func(t *testing.T, out string) {
				u, err := url.Parse(out)
				if err != nil {
					t.Fatalf("reparse %q: %v", out, err)
				}
				if u.Scheme != "postgres" || u.Host != "localhost:5432" || u.Path != "/ragdb" {
					t.Errorf("scheme/host/path = %q/%q/%q, want postgres/localhost:5432//ragdb", u.Scheme, u.Host, u.Path)
				}
				if got := u.Query().Get("sslmode"); got != "disable" {
					t.Errorf("sslmode = %q, want %q (existing params must survive)", got, "disable")
				}
				if got := u.Query().Get("search_path"); got != "rag_test_deadbeef,public" {
					t.Errorf("search_path = %q, want %q", got, "rag_test_deadbeef,public")
				}
			},
		},
		{
			name: "overwrites a pre-existing search_path instead of appending to it",
			in:   "postgres://rag:rag@localhost:5432/ragdb?search_path=whatever_the_dev_had",
			check: func(t *testing.T, out string) {
				u, err := url.Parse(out)
				if err != nil {
					t.Fatalf("reparse %q: %v", out, err)
				}
				got := u.Query()["search_path"]
				if len(got) != 1 || got[0] != "rag_test_deadbeef,public" {
					t.Errorf("search_path = %v, want a single value %q (old value must not survive alongside it)",
						got, "rag_test_deadbeef,public")
				}
			},
		},
		{
			name:    "rejects a libpq keyword/value DSN",
			in:      "host=localhost user=rag password=rag dbname=ragdb port=5433 sslmode=disable",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := withSearchPath(t, tc.in, "rag_test_deadbeef,public")
			if tc.wantErr {
				if err == nil {
					t.Errorf("withSearchPath(%q) = nil error, want one", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("withSearchPath(%q): %v", tc.in, err)
			}
			tc.check(t, out)
		})
	}
}

// TestRandomSchemaNameFormat checks the name matches schemaNameRE: both the
// DROP SCHEMA cleanup and "safe to interpolate unquoted" depend on it.
func TestRandomSchemaNameFormat(t *testing.T) {
	name := randomSchemaName(t)
	if !schemaNameRE.MatchString(name) {
		t.Errorf("schema name %q does not match %s", name, schemaNameRE)
	}
}

// ChunkTableDDL must never schema-qualify its name, or putting the
// throwaway schema first on search_path would not isolate it from public.
func TestChunkTableDDLNeverSchemaQualifiesName(t *testing.T) {
	for _, ifNotExists := range []bool{true, false} {
		for _, name := range []string{chunksTable, stagingTable} {
			ddl := ChunkTableDDL(name, ifNotExists)
			if strings.Contains(ddl, "public") {
				t.Errorf("ChunkTableDDL(%q, %v) mentions \"public\": %s", name, ifNotExists, ddl)
			}
			if !strings.Contains(ddl, "CREATE TABLE") {
				t.Errorf("ChunkTableDDL(%q, %v) missing CREATE TABLE: %s", name, ifNotExists, ddl)
			}
		}
	}
}
