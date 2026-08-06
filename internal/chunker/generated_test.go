package chunker

import (
	"fmt"
	"strings"
	"testing"
)

func TestVersionNum(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int16
	}{
		{"v3.0", 30},
		{"3.2", 32},
		{"v3.10", 40}, // major*10+minor is the existing convention, kept as-is
		{"", 0},
		{"v3", 0},
		{"fctl-2.0", 0}, // another component's label must not read as a stack version
		{"sdk-3.1", 0},
	} {
		if got := versionNum(tc.in); got != tc.want {
			t.Errorf("versionNum(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// An operation that never changes across the three stack ranges must produce a
// single chunk spanning them all, not three copies. This is what keeps the
// corpus — and the embedding time — from tripling.
func TestParseGeneratedJSONCoalescesIdenticalRanges(t *testing.T) {
	src := `{"api.operations":{"__component":"stack","__versions":[
	  {"range":{"min":"v3.0","below":"v3.1"},"data":{"getInfo":{"method":"GET","path":"/info","summary":"Server info"}}},
	  {"range":{"min":"v3.1","below":"v3.2"},"data":{"getInfo":{"method":"GET","path":"/info","summary":"Server info"}}},
	  {"range":{"min":"v3.2"},"data":{"getInfo":{"method":"GET","path":"/info","summary":"Server info"}}}
	]}}`

	chunks := ParseGeneratedJSON("apps/docs/content/data/generated/api.operations.json", []byte(src))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1 (identical across ranges)", len(chunks))
	}
	c := chunks[0]
	if c.VersionNum != 30 {
		t.Errorf("VersionNum = %d, want 30", c.VersionNum)
	}
	if c.VersionBelow != 0 {
		t.Errorf("VersionBelow = %d, want 0 (open-ended: last range has no upper bound)", c.VersionBelow)
	}
	if !strings.HasPrefix(c.Content, "GET /info — Server info") {
		t.Errorf("content should lead with method, path and summary, got:\n%s", c.Content)
	}
}

// When the content does change, each distinct span keeps its own bounds so a
// question scoped to one version cannot retrieve another version's answer.
func TestParseGeneratedJSONSplitsOnChangedContent(t *testing.T) {
	src := `{"api.operations":{"__component":"stack","__versions":[
	  {"range":{"min":"v3.0","below":"v3.1"},"data":{"createTx":{"method":"POST","path":"/tx","summary":"Old"}}},
	  {"range":{"min":"v3.1","below":"v3.2"},"data":{"createTx":{"method":"POST","path":"/tx","summary":"Old"}}},
	  {"range":{"min":"v3.2"},"data":{"createTx":{"method":"POST","path":"/transactions","summary":"New"}}}
	]}}`

	chunks := ParseGeneratedJSON("x.json", []byte(src))
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	// First span covers v3.0 and v3.1, stopping below v3.2.
	if chunks[0].VersionNum != 30 || chunks[0].VersionBelow != 32 {
		t.Errorf("first span = [%d,%d), want [30,32)", chunks[0].VersionNum, chunks[0].VersionBelow)
	}
	if chunks[1].VersionNum != 32 || chunks[1].VersionBelow != 0 {
		t.Errorf("second span = [%d,%d), want [32,open)", chunks[1].VersionNum, chunks[1].VersionBelow)
	}
}

// api.fctl has no version composition: a flat map of entries, with per-entry
// labels from another component that must not be read as stack versions.
func TestParseGeneratedJSONFlatShape(t *testing.T) {
	src := `{"api.fctl":{"v2CreateLedger":{"command":"fctl ledger create {ledger}","minVersion":"fctl-2.0"}}}`

	chunks := ParseGeneratedJSON("apps/docs/content/data/generated/api.fctl.json", []byte(src))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	c := chunks[0]
	if c.VersionNum != 0 || c.VersionBelow != 0 {
		t.Errorf("bounds = [%d,%d), want unbounded", c.VersionNum, c.VersionBelow)
	}
	if !strings.HasPrefix(c.Content, "fctl ledger create {ledger}") {
		t.Errorf("content should lead with the command, got:\n%s", c.Content)
	}
	if !strings.Contains(c.Content, "Available from: fctl-2.0") {
		t.Errorf("content should keep the fctl version label, got:\n%s", c.Content)
	}
}

// Unknown datasets still have to be searchable, via labelled key/value lines.
func TestParseGeneratedJSONGenericFallback(t *testing.T) {
	src := `{"ledger.features":{"__component":"stack","__versions":[
	  {"range":{"min":"v3.2"},"data":{"HASH_LOGS":{"default":"SYNC","values":["SYNC","DISABLED"]}}}
	]}}`

	chunks := ParseGeneratedJSON("y.json", []byte(src))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	got := chunks[0].Content
	for _, want := range []string{"HASH_LOGS", "default: SYNC", "values: SYNC, DISABLED"} {
		if !strings.Contains(got, want) {
			t.Errorf("content missing %q, got:\n%s", want, got)
		}
	}
}

// The RBAC scopes, the OTel metrics and the audit events are plain lists. The
// pages that show them are one-line <DocDataTable> wrappers with no prose of
// their own, so skipping this shape put the content nowhere in the index.
func TestParseGeneratedJSONListShape(t *testing.T) {
	src := `{"membership.scopes":[
	  {"label":"stack:Read","category":"Stack","description":"Read-only access"},
	  {"label":"stack:Write","category":"Stack","description":"Write access"}
	]}`

	chunks := ParseGeneratedJSON("apps/docs/content/data/generated/membership.scopes.json", []byte(src))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want the whole list in 1", len(chunks))
	}
	got := chunks[0].Content
	for _, want := range []string{"membership.scopes", "- stack:Read — category: Stack; description: Read-only access", "- stack:Write"} {
		if !strings.Contains(got, want) {
			t.Errorf("content missing %q, got:\n%s", want, got)
		}
	}
	if chunks[0].Section != "membership.scopes" {
		t.Errorf("section = %q, want the dataset name", chunks[0].Section)
	}
}

// A list is kept whole while it fits: retrieval admits two chunks per file, so
// a list split into several parts could never come back complete.
func TestParseGeneratedJSONListSplitsOnlyWhenTooLarge(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"big.list":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"entry-%03d","description":"a description long enough to matter"}`, i)
	}
	b.WriteString(`]}`)

	chunks := ParseGeneratedJSON("apps/docs/content/data/generated/big.list.json", []byte(b.String()))
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want the oversized list split", len(chunks))
	}
	for i, c := range chunks {
		if len(c.Content) > maxDataChunkBytes {
			t.Errorf("chunk %d is %d bytes, over the %d limit", i, len(c.Content), maxDataChunkBytes)
		}
		if !strings.HasPrefix(c.Content, "big.list\n\n") {
			t.Errorf("chunk %d should name its dataset so it reads standalone, got:\n%.60s", i, c.Content)
		}
	}
}

// Scalars are one-line datasets — the minimum Kubernetes version, the Helm
// chart URL. The value alone matches nothing, so the name has to travel with it.
func TestParseGeneratedJSONScalarShape(t *testing.T) {
	chunks := ParseGeneratedJSON("z.json", []byte(`{"infra.k8s-min-version":"1.30"}`))
	if len(chunks) != 1 {
		t.Fatalf("got %d chunks, want 1", len(chunks))
	}
	got := chunks[0].Content
	if !strings.Contains(got, "infra.k8s-min-version") || !strings.Contains(got, "1.30") {
		t.Errorf("content should carry both the name and the value, got:\n%s", got)
	}
}

func TestParseGeneratedJSONIgnoresGarbage(t *testing.T) {
	if got := ParseGeneratedJSON("x.json", []byte("not json")); got != nil {
		t.Errorf("want nil for invalid JSON, got %d chunks", len(got))
	}
}
