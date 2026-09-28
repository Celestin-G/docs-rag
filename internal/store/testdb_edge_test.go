package store

import (
	"net/url"
	"strings"
	"testing"
)

// These exercise withSearchPath, randomSchemaName and ChunkTableDDL directly.
// They are pure functions (no network), so — unlike the rest of this
// package's tests — they run even when TEST_DATABASE_URL is unset. That
// matters because this is exactly the code responsible for keeping store
// tests off the developer's real index; it deserves coverage that doesn't
// itself depend on a database being reachable.

// TestWithSearchPathPreservesSchemeHostPathAndExistingParams checks that
// adding search_path does not disturb the rest of a standard URL-form DSN,
// and that an unrelated existing query parameter survives alongside it.
func TestWithSearchPathPreservesSchemeHostPathAndExistingParams(t *testing.T) {
	in := "postgres://rag:rag@localhost:5432/ragdb?sslmode=disable"
	out, err := withSearchPath(t, in, "rag_test_deadbeef,public")
	if err != nil {
		t.Fatalf("withSearchPath: %v", err)
	}

	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("reparse %q: %v", out, err)
	}
	if u.Scheme != "postgres" {
		t.Errorf("scheme = %q, want %q", u.Scheme, "postgres")
	}
	if u.Host != "localhost:5432" {
		t.Errorf("host = %q, want %q", u.Host, "localhost:5432")
	}
	if u.Path != "/ragdb" {
		t.Errorf("path = %q, want %q", u.Path, "/ragdb")
	}
	if got := u.Query().Get("sslmode"); got != "disable" {
		t.Errorf("sslmode = %q, want %q (existing params must survive)", got, "disable")
	}
	if got := u.Query().Get("search_path"); got != "rag_test_deadbeef,public" {
		t.Errorf("search_path = %q, want %q", got, "rag_test_deadbeef,public")
	}
}

// TestWithSearchPathOverwritesPreexistingSearchPath checks that a
// TEST_DATABASE_URL which already carries a search_path (e.g. a developer's
// own default) ends up with exactly the throwaway schema's value, not both
// values concatenated or the old one winning.
func TestWithSearchPathOverwritesPreexistingSearchPath(t *testing.T) {
	in := "postgres://rag:rag@localhost:5432/ragdb?search_path=whatever_the_dev_had"
	out, err := withSearchPath(t, in, "rag_test_deadbeef,public")
	if err != nil {
		t.Fatalf("withSearchPath: %v", err)
	}

	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("reparse %q: %v", out, err)
	}
	got := u.Query()["search_path"]
	if len(got) != 1 || got[0] != "rag_test_deadbeef,public" {
		t.Errorf("search_path = %v, want a single value %q (old value must not survive alongside the new one)",
			got, "rag_test_deadbeef,public")
	}
}

// TestWithSearchPathPreservesSocketStyleQueryParams checks the libpq form
// that has no host in the authority at all (unix socket via a "host" query
// param, e.g. postgres:///dbname?host=/var/run/postgresql) — plausible for a
// developer running Postgres locally without Docker.
func TestWithSearchPathPreservesSocketStyleQueryParams(t *testing.T) {
	in := "postgres:///ragdb?host=%2Fvar%2Frun%2Fpostgresql&user=rag"
	out, err := withSearchPath(t, in, "rag_test_deadbeef,public")
	if err != nil {
		t.Fatalf("withSearchPath: %v", err)
	}

	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("reparse %q: %v", out, err)
	}
	if got := u.Query().Get("host"); got != "/var/run/postgresql" {
		t.Errorf("host param = %q, want %q", got, "/var/run/postgresql")
	}
	if got := u.Query().Get("user"); got != "rag" {
		t.Errorf("user param = %q, want %q", got, "rag")
	}
	if got := u.Query().Get("search_path"); got != "rag_test_deadbeef,public" {
		t.Errorf("search_path = %q, want %q", got, "rag_test_deadbeef,public")
	}
}

// TestWithSearchPathPreservesPercentEncodedCredentials checks a password
// containing characters that need percent-encoding in a URL (e.g. "@") is
// not corrupted by the round trip through url.Parse/url.Values/u.String.
func TestWithSearchPathPreservesPercentEncodedCredentials(t *testing.T) {
	in := "postgres://rag:p%40ss@localhost:5432/ragdb"
	out, err := withSearchPath(t, in, "rag_test_deadbeef,public")
	if err != nil {
		t.Fatalf("withSearchPath: %v", err)
	}

	u, err := url.Parse(out)
	if err != nil {
		t.Fatalf("reparse %q: %v", out, err)
	}
	pass, ok := u.User.Password()
	if !ok || pass != "p@ss" {
		t.Errorf("password = %q (ok=%v), want %q", pass, ok, "p@ss")
	}
}

// TestWithSearchPathRejectsKeywordValueDSN checks that withSearchPath
// rejects libpq keyword/value form DSNs (e.g. "host=... user=... dbname=...")
// with a clear error message, rather than silently producing an invalid string
// that would neither be a valid URL nor valid keyword/value form.
func TestWithSearchPathRejectsKeywordValueDSN(t *testing.T) {
	in := "host=localhost user=rag password=rag dbname=ragdb port=5433 sslmode=disable"
	_, err := withSearchPath(t, in, "rag_test_deadbeef,public")

	if err == nil {
		t.Errorf("withSearchPath should reject keyword/value DSN %q, but returned nil error", in)
	}
}

// TestRandomSchemaNameFormatAndCharset checks the generated name matches the
// "rag_test_<16 lowercase hex chars>" contract that both the DROP SCHEMA
// cleanup and the "safe to interpolate unquoted" claim in the doc comment
// depend on.
func TestRandomSchemaNameFormatAndCharset(t *testing.T) {
	name := randomSchemaName(t)
	const prefix = "rag_test_"
	if !strings.HasPrefix(name, prefix) {
		t.Fatalf("schema name %q missing prefix %q", name, prefix)
	}
	hexPart := strings.TrimPrefix(name, prefix)
	if len(hexPart) != 16 {
		t.Errorf("hex part of %q has length %d, want 16", name, len(hexPart))
	}
	for _, r := range hexPart {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Errorf("schema name %q contains non-hex character %q — unsafe to interpolate unquoted", name, r)
		}
	}
	if len(name) > 63 {
		t.Errorf("schema name %q is %d bytes, over Postgres's 63-byte identifier limit", name, len(name))
	}
}

// TestRandomSchemaNameNoCollisionsAcrossManyCalls is a cheap sanity check
// that randomSchemaName is actually reading fresh randomness per call rather
// than e.g. a fixed or poorly-seeded source — a collision here would mean
// two parallel test runs could share (and CASCADE-drop) the same schema.
func TestRandomSchemaNameNoCollisionsAcrossManyCalls(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		name := randomSchemaName(t)
		if seen[name] {
			t.Fatalf("randomSchemaName produced a duplicate after %d calls: %q", i, name)
		}
		seen[name] = true
	}
}

// TestChunkTableDDLNeverSchemaQualifiesName guards the isolation invariant
// testStore's doc comment relies on: ChunkTableDDL must emit CREATE TABLE
// against whatever bare name it is given, never a "public."-qualified one —
// otherwise putting the throwaway schema first on search_path would not be
// enough to keep it out of the developer's public.doc_chunks.
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
