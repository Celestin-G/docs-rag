package kbpublish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

type fakeDates map[string]string

func (f fakeDates) Date(rel string) (string, bool) {
	if d, ok := f[rel]; ok {
		return d, true
	}
	return "2026-01-01", false
}

func TestGithubSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Hello World":             "hello-world",
		"What's new?":             "whats-new",
		"balance(account, asset)": "balanceaccount-asset",
		"A  B":                    "a--b",
		"snake_case & more":       "snake_case--more",
		"Über café":               "über-café",
		"API v2.1":                "api-v21",
	} {
		if got := githubSlug(in); got != want {
			t.Errorf("githubSlug(%q) = %q, want %q", in, got, want)
		}
	}
	g := newSlugger()
	if a, b, c := g.slug("Setup"), g.slug("Setup"), g.slug("Setup"); a != "setup" || b != "setup-1" || c != "setup-2" {
		t.Errorf("dedup gave %q %q %q", a, b, c)
	}
}

func TestPageHeadings(t *testing.T) {
	raw := "---\ntitle: X\n---\n# Top\n\n## Setup\n\n```bash\n## not a heading\n```\n\n### `balance()` and **bold**\n\n## Setup ##\n\n## [Link](/x) text\n"
	var got []string
	for _, h := range pageHeadings(raw) {
		got = append(got, h.raw+"=>"+h.id)
	}
	want := []string{"Top=>top", "Setup=>setup", "`balance()` and **bold**=>balance-and-bold", "Setup ##=>setup-1", "[Link](/x) text=>link-text"}
	if !slices.Equal(got, want) {
		t.Errorf("headings = %q\nwant      %q", got, want)
	}
}

func TestKeySlug(t *testing.T) {
	long := strings.Repeat("abcdefghij ", 12)
	for _, tc := range []struct{ in, want string }{
		{"Overview > Setup", "overview-setup"},
		{"  Hello, Wörld!  ", "hello-w-rld"},
		{"v3.1 notes", "v3.1-notes"},
		{"..", "section"},
		{"", "section"},
		{"!!!", "section"},
	} {
		if got := keySlug(tc.in); got != tc.want {
			t.Errorf("keySlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	a, b := keySlug(long+"one"), keySlug(long+"two")
	if a == b || len(a) > 60+1+8 || !strings.HasPrefix(a, "abcdefghij-abcdefghij") {
		t.Errorf("truncation: %q vs %q", a, b)
	}
	if keySlug(long+"one") != a {
		t.Error("truncated slug is not deterministic")
	}
	if len(keySlug(strings.Repeat("a", 60))) != 60 {
		t.Error("a 60-character slug must not be truncated")
	}
}

func TestProducts(t *testing.T) {
	for in, want := range map[string]string{
		"/modules/ledger/accounts":    "ledger",
		"/modules/flows/x":            "flows",
		"/modules/orchestration/x":    "flows",
		"/modules/webhooks/x":         "webhooks",
		"/modules/auth/x":             "other",
		"/modules":                    "other",
		"/deploy/cloud":               "stack",
		"/getting-started/quickstart": "stack",
		"/stack-api-reference/":       "stack",
		"/changelog/v3.1":             "other",
		"/":                           "other",
		"":                            "other",
	} {
		if got := productFromPath(in); got != want {
			t.Errorf("productFromPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"ledger.events": "ledger", "payments.connectors": "payments", "numscript.snippets": "numscript",
		"stack.oas": "stack", "api.operations": "stack", "sdks": "stack", "audit.module-fields": "stack",
		"membership.oas": "other", "mystery.x": "other",
	} {
		if got := generatedProduct(in); got != want {
			t.Errorf("generatedProduct(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitBody(t *testing.T) {
	para := strings.Repeat("word ", 40) // 200 bytes
	body := strings.Join([]string{para, para, para, para}, "\n\n")
	parts := splitBody(body, 450)
	if len(parts) != 2 {
		t.Fatalf("paragraph split: %d parts, want 2", len(parts))
	}
	lines := strings.Repeat(strings.Repeat("x", 90)+"\n", 10)
	if parts := splitBody(lines, 300); len(parts) != 4 {
		t.Errorf("line split: %d parts, want 4", len(parts))
	}
	long := strings.Repeat("é", 500) // one 1000-byte line, no space
	for _, p := range splitBody(long, 300) {
		if len(p) > 300 || strings.ContainsRune(p, '�') {
			t.Fatalf("hard cut produced a bad part of %d bytes", len(p))
		}
	}
	if strings.Join(splitBody(long, 300), "") != long {
		t.Error("hard cut lost text")
	}
}

func TestSplitBodyKeepsFencesBalanced(t *testing.T) {
	code := strings.Repeat("let x = 1\n\nlet y = 2\n", 30) // blank lines inside the block
	text := "Intro paragraph.\n\n```go title=\"a.go\"\n" + code + "```\n\nOutro."
	parts := splitBody(text, 300)
	if len(parts) < 3 {
		t.Fatalf("expected a re-split, got %d parts", len(parts))
	}
	for i, p := range parts {
		if len(p) > 300 {
			t.Errorf("part %d is %d bytes", i, len(p))
		}
		if n := strings.Count(p, "\n```") + strings.Count(p[:3], "```"); n%2 != 0 {
			t.Errorf("part %d has an unbalanced fence:\n%s", i, p)
		}
	}
	if !strings.HasPrefix(parts[1], "```go title=\"a.go\"\n") {
		t.Errorf("second part does not reopen the fence with its info string:\n%s", parts[1])
	}
	if last := parts[len(parts)-1]; !strings.HasSuffix(last, "Outro.") || strings.Count(strings.Join(parts, "\n"), "```")%2 != 0 {
		t.Errorf("last part = %q", last)
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		glob, name string
		want       bool
	}{
		{"apps/docs/content/data/generated/stack.oas*.json", "apps/docs/content/data/generated/stack.oas.json", true},
		{"apps/docs/content/data/generated/stack.oas*.json", "apps/docs/content/data/generated/stack.oas.prose.json", true},
		{"apps/docs/content/data/generated/stack.oas*.json", "apps/docs/content/data/generated/stack.events.json", false},
		{"apps/*/x.mdx", "apps/a/b/x.mdx", false},
		{"apps/**/x.mdx", "apps/a/b/x.mdx", true},
		{"a?.md", "ab.md", true},
		{"a?.md", "a/.md", false},
	} {
		if got := globMatch(tc.glob, tc.name); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v", tc.glob, tc.name, got)
		}
	}
	if !excluded("apps/docs/content/pages/x/stack.oas.json", []string{"stack.oas*.json"}) {
		t.Error("a pattern without a slash must match the file name")
	}
	if excluded("a/b.md", nil) {
		t.Error("no pattern excludes nothing")
	}
}

func TestParseGitLog(t *testing.T) {
	log := "\x012026-10-02\na.mdx\nb/c.mdx\n\n\x012026-09-01\na.mdx\nd.json\n"
	got, err := parseGitLog(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{"a.mdx": "2026-10-02", "b/c.mdx": "2026-10-02", "d.json": "2026-09-01"} {
		if got[f] != want {
			t.Errorf("%s = %q, want %q (the latest commit wins)", f, got[f], want)
		}
	}
	if _, err := parseGitLog(strings.NewReader("\x01yesterday\n")); err == nil {
		t.Error("a malformed date must be an error")
	}
}

const pageFixture = `---
path: /modules/ledger/accounts
title: "Accounts"
component: ledger
version: { min: "3.0.0" }
badge: enterprise
---

Intro text.

## Setup

First setup.

<When component="stack" below="v4.0">

Legacy note.

</When>

## Setup

Second setup.

### ` + "`balance()`" + ` details

Detail text.

## Big

BIGBODY
`

func buildFixture(t *testing.T) *Plan {
	t.Helper()
	big := strings.TrimSpace(strings.Repeat("A paragraph of filler text that is long enough to matter.\n\n", 60))
	sources := []SourceFile{
		{Rel: "apps/docs/content/pages/a.mdx", Content: []byte(strings.Replace(pageFixture, "BIGBODY", big, 1))},
		{Rel: "apps/docs/content/data/generated/ledger.events.json", Content: []byte(`{
		  "ledger.events": {"__component": "ledger", "__versions": [
		    {"range": {"min": "2.3.0", "below": "2.4.0"}, "data": {"B": "second", "A": "first"}}]},
		  "ledger.other": {"X": "x value"}}`)},
		{Rel: "apps/docs/content/fragments/ledger/frag.mdx", Content: []byte("---\nid: ledger/frag\n---\n\n## Part\n\nFragment text.\n")},
		{Rel: "apps/docs/content/pages/b.mdx", Content: []byte("---\npath: /modules/ledger/b\ntitle: B\n---\n\n<Fragment id=\"ledger/frag\" />\n\n## T\n\ntext\n")},
		{Rel: "README.md", Content: []byte("ignored")},
	}
	return Build(sources, Options{Table: mustTable(t), Dates: fakeDates{"apps/docs/content/pages/a.mdx": "2026-10-01"}})
}

func TestBuildMapping(t *testing.T) {
	p := buildFixture(t)
	if len(p.Errors) != 0 {
		t.Fatalf("validation errors: %v", p.Errors)
	}
	byKey := map[string]File{}
	var keys []string
	for _, f := range p.Files {
		byKey[f.Key] = f
		keys = append(keys, f.Key)
	}
	for _, want := range []string{
		"docs/ledger/modules/ledger/accounts/_intro.md",
		"docs/ledger/modules/ledger/accounts/setup.md",
		"docs/ledger/modules/ledger/accounts/setup-2.md",
		"docs/ledger/modules/ledger/accounts/setup-balance-details.md",
		"docs/ledger/modules/ledger/accounts/big.md",
		"docs/ledger/modules/ledger/accounts/big-p2.md",
		"docs/ledger/_fragments/ledger/frag/part.md",
		"docs/ledger/_data/ledger.events/a.md",
		"docs/ledger/_data/ledger.events/b.md",
		"docs/ledger/_data/ledger.other/x.md",
	} {
		if _, ok := byKey[want]; !ok {
			t.Errorf("missing key %s in %v", want, keys)
		}
	}
	if !slices.IsSorted(keys) {
		t.Error("files are not sorted by key")
	}

	intro := string(byKey["docs/ledger/modules/ledger/accounts/_intro.md"].Body)
	if !strings.HasPrefix(intro, "Accounts\nApplies to: Stack v4.0 and later (ledger 3.0.0 and later); Enterprise Edition\n\nIntro text.") {
		t.Errorf("intro body = %q", intro)
	}
	setup := string(byKey["docs/ledger/modules/ledger/accounts/setup.md"].Body)
	if !strings.Contains(setup, "(Applies before Stack v4.0)\n\nLegacy note.") {
		t.Errorf("inline note missing: %q", setup)
	}
	for _, f := range p.Files {
		if strings.Contains(f.Key, "/big") && !strings.HasPrefix(string(f.Body), "Accounts > Big\nApplies to:") {
			t.Errorf("%s does not repeat the breadcrumb and header: %.60q", f.Key, f.Body)
		}
		if len(f.Body) > maxFileBytes {
			t.Errorf("%s is %d bytes", f.Key, len(f.Body))
		}
	}

	side := string(byKey["docs/ledger/modules/ledger/accounts/setup-2.md"].Sidecar)
	var sc struct {
		Attrs map[string]any `json:"metadataAttributes"`
	}
	if err := json.Unmarshal([]byte(side), &sc); err != nil || len(sc.Attrs) != 11 || sc.Attrs["version"] != nil {
		t.Errorf("the sidecar must have 11 keys and no legacy version key: %v %s", err, side)
	}
	for _, want := range []string{
		`"source_url":"https://docs.formance.com/modules/ledger/accounts#setup-1"`, // second heading of that name
		`"license":"ee"`, `"version_min":400`, `"version_below":99999`, `"version_component":"ledger"`,
		`"updated_at":"2026-10-01"`, `"product":"ledger"`, `"deployment":"any"`,
	} {
		if !strings.Contains(side, want) {
			t.Errorf("sidecar lacks %s: %s", want, side)
		}
	}
	if s := string(byKey["docs/ledger/modules/ledger/accounts/setup-balance-details.md"].Sidecar); !strings.Contains(s, "accounts#balance-details") {
		t.Errorf("leaf anchor wrong: %s", s)
	}
	if s := string(byKey["docs/ledger/_fragments/ledger/frag/part.md"].Sidecar); !strings.Contains(s, `"source_url":"https://docs.formance.com/modules/ledger/b"`) {
		t.Errorf("fragment url: %s", s)
	}
	if s := string(byKey["docs/ledger/_data/ledger.events/a.md"].Sidecar); !strings.Contains(s, `"version_min":301`) || !strings.Contains(s, `"version_below":302`) {
		t.Errorf("generated bounds: %s", s)
	}

	r := p.Report
	if r.FilesByKind["pages"] != 2 || r.FilesByKind["generated"] != 1 || r.FilesByKind["fragments"] != 1 {
		t.Errorf("files by kind = %v", r.FilesByKind)
	}
	if r.WhenNotes != 1 || r.ResplitChunks == 0 || r.Headers == 0 || r.MaxSidecarBytes == 0 {
		t.Errorf("report = %+v", r)
	}
	if st := r.ByDataset["apps/docs/content/data/generated/ledger.events.json ledger.events"]; st.Files != 2 {
		t.Errorf("dataset stat = %+v", st)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, b := buildFixture(t), buildFixture(t)
	if len(a.Files) != len(b.Files) {
		t.Fatal("different file counts")
	}
	for i := range a.Files {
		if a.Files[i].Key != b.Files[i].Key || a.Files[i].Hash != b.Files[i].Hash {
			t.Fatalf("run differs at %s", a.Files[i].Key)
		}
	}
}

func TestBuildCollectsErrors(t *testing.T) {
	page := func(path string) []byte {
		return []byte("---\npath: " + path + "\ntitle: T\n---\n\n## S\n\ntext\n")
	}
	p := Build([]SourceFile{
		{Rel: "apps/docs/content/pages/a.mdx", Content: page("/same")},
		{Rel: "apps/docs/content/pages/b.mdx", Content: page("/same")},
		{Rel: "apps/docs/content/pages/c.mdx", Content: []byte("---\npath: /c?x=1\ntitle: T\n---\n\n## S\n\ntext\n")},
	}, Options{Table: mustTable(t), Dates: fakeDates{}})
	joined := strings.Join(p.Errors, "\n")
	if !strings.Contains(joined, "duplicate key") {
		t.Errorf("duplicate key not detected: %s", joined)
	}
	if len(p.Report.URLFallbacks) == 0 {
		t.Errorf("a path with a query must fall back to the site root")
	}
	if p.Report.DateFallbacks != 3 {
		t.Errorf("date fallbacks = %d, want 3", p.Report.DateFallbacks)
	}
}

func TestBuildExcludes(t *testing.T) {
	src := []SourceFile{{Rel: "apps/docs/content/data/generated/stack.oas.json", Content: []byte(`{"stack.oas": {"k": "v"}}`)}}
	if p := Build(src, Options{Table: mustTable(t), Dates: fakeDates{}, Excludes: []string{"**/stack.oas*.json"}}); len(p.Files) != 0 || p.Report.Excluded != 1 {
		t.Errorf("excluded file still built: %+v", p.Report)
	}
}

// A frontmatter path is author input: dot segments, a leading "docs" and
// uppercase must not move a key out of docs/<product>/ or break the charset.
func TestBuildKeysStayUnderDocs(t *testing.T) {
	page := func(path string) []byte {
		return []byte("---\npath: " + path + "\ntitle: T\n---\n\n## S\n\ntext\n")
	}
	p := Build([]SourceFile{
		{Rel: "apps/docs/content/pages/a.mdx", Content: page("/../../etc/passwd")},
		{Rel: "apps/docs/content/pages/b.mdx", Content: page("/./x/../Y Z")},
		{Rel: "apps/docs/content/pages/c.mdx", Content: page("..")},
		{Rel: "apps/docs/content/fragments/../../../../x.mdx", Content: []byte("## S\n\ntext\n")},
	}, Options{Table: mustTable(t), Dates: fakeDates{}})
	if len(p.Errors) != 0 {
		t.Fatalf("errors: %v", p.Errors)
	}
	if len(p.Files) < 3 {
		t.Fatalf("got %d files", len(p.Files))
	}
	for _, f := range p.Files {
		if !strings.HasPrefix(f.Key, "docs/other/") || len(ValidateKey(f.Key)) != 0 {
			t.Errorf("key %q escapes docs/other/ or is invalid: %v", f.Key, ValidateKey(f.Key))
		}
	}
}

func TestDefaultExcludes(t *testing.T) {
	for rel, want := range map[string]bool{
		"apps/docs/content/data/generated/stack.oas.json":             true,
		"apps/docs/content/data/generated/stack.oas.enriched.json":    true,
		"apps/docs/content/data/generated/stack.oas.prose.json":       true,
		"apps/docs/content/data/generated/membership.oas.json":        true,
		"apps/docs/content/data/generated/api.schemas.json":           true,
		"apps/docs/content/data/generated/ledger.cli-flags.json":      false,
		"apps/docs/content/data/generated/api.operations.json":        false,
		"apps/docs/content/data/generated/membership.operations.json": false,
	} {
		if got := excluded(rel, DefaultExcludes); got != want {
			t.Errorf("excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

// A range no Stack release ships is left out, never widened; a component that
// leaves the Stack is capped at the first release without it.
func TestBuildUnshippedAndCapped(t *testing.T) {
	page := func(path, front, body string) []byte {
		return []byte("---\npath: " + path + "\ntitle: T\n" + front + "---\n\n" + body)
	}
	p := Build([]SourceFile{
		{Rel: "apps/docs/content/pages/empty.mdx", Content: page("/modules/payments/empty", "component: payments\nversion: { min: \"3.2.0\", below: \"3.3.0\" }\n", "Intro.\n\n## A\n\ntext a\n\n## B\n\ntext b\n")},
		{Rel: "apps/docs/content/pages/inline.mdx", Content: page("/modules/payments/inline", "", "## S\n\nKept text.\n\n<When component=\"payments\" minimum=\"3.2.0\" below=\"3.3.0\">\n\nDropped text.\n\n</When>\n")},
		{Rel: "apps/docs/content/pages/capped.mdx", Content: page("/modules/payments/capped", "component: payments\n", "## S\n\ntext\n")},
	}, Options{Table: mustTable(t), Dates: fakeDates{}})
	if len(p.Errors) != 0 {
		t.Fatalf("errors: %v", p.Errors)
	}
	r := p.Report
	if len(r.Widened) != 0 || len(r.Unshipped) != 2 || r.UnshippedChunks != 3 || r.UnshippedBlocks != 1 {
		t.Errorf("widened %v, unshipped %v, chunks %d, blocks %d", r.Widened, r.Unshipped, r.UnshippedChunks, r.UnshippedBlocks)
	}
	if x := r.Unshipped[0]; x.File != "apps/docs/content/pages/empty.mdx" || x.Inline || x.Component != "payments" || x.Bound != "min 3.2.0, below 3.3.0" {
		t.Errorf("exclusion = %+v", x)
	}
	for _, f := range p.Files {
		body, side := string(f.Body), string(f.Sidecar)
		switch {
		case strings.Contains(f.Key, "/empty/"):
			t.Errorf("an empty range was published: %s", f.Key)
		case strings.Contains(body, "Dropped text"):
			t.Errorf("the dropped block survived: %s", body)
		case strings.Contains(f.Key, "/capped/"):
			if !strings.Contains(side, `"version_below":400`) || !strings.Contains(body, "Applies to: before Stack v4.0") {
				t.Errorf("capped page: %s %s", body, side)
			}
		}
	}
	var out strings.Builder
	r.PrintSummary(&out, nil)
	if !strings.Contains(out.String(), "EXCLUDED: no Stack release ships these ranges: 2 entries (1 page-level, 1 inline); 3 chunks and 1 inline blocks") {
		t.Errorf("summary:\n%s", out.String())
	}
}

func TestBuildDatasetExcludes(t *testing.T) {
	src := []SourceFile{{Rel: "apps/docs/content/data/generated/numscript.snippets.json", Content: []byte(`{"ds.keep": {"A": "a"}, "ds.drop": {"B": "b", "C": "c"}}`)}}
	for _, tc := range []struct {
		name     string
		excludes []string
		files    int
		dropped  int
	}{
		{"none", nil, 3, 0},
		{"one dataset", []string{"ds.drop"}, 1, 2},
		{"glob", []string{"ds.*"}, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := Build(src, Options{Table: mustTable(t), Dates: fakeDates{}, DatasetExcludes: tc.excludes})
			total := 0
			for _, n := range p.Report.DatasetExcluded {
				total += n
			}
			if len(p.Files) != tc.files || total != tc.dropped {
				t.Errorf("files %d (want %d), dataset chunks dropped %d (want %d)", len(p.Files), tc.files, total, tc.dropped)
			}
		})
	}
	if !excluded("cookbook.numscript.byFingerprint", DefaultDatasetExcludes) || excluded("cookbook.numscript.snippets", DefaultDatasetExcludes) {
		t.Errorf("default dataset excludes = %v", DefaultDatasetExcludes)
	}
}

// fixtureFile reads a published file that failed ingestion and returns its
// breadcrumb/header prefix (the first prefixLines lines, or fallback) and body.
func fixtureFile(t *testing.T, name string, prefixLines int, fallback string) (prefix, body string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if prefixLines == 0 {
		return fallback, text
	}
	lines := strings.SplitN(text, "\n", prefixLines+1)
	return strings.Join(lines[:prefixLines], "\n"), strings.TrimLeft(lines[prefixLines], "\n")
}

func TestRejectedFixturesSplitUnderTokenBudget(t *testing.T) {
	for _, tc := range []struct {
		file        string
		prefixLines int
		fallback    string
	}{
		{"numscript-snippets-27-p3.md", 0, "S"},
		{"combine-conditions-2.md", 2, ""},
	} {
		t.Run(tc.file, func(t *testing.T) {
			prefix, body := fixtureFile(t, tc.file, tc.prefixLines, tc.fallback)
			if n := estimateTokens(prefix + "\n\n" + body); n <= maxFileTokens {
				t.Fatalf("fixture is estimated at %d tokens, want > %d", n, maxFileTokens)
			}
			parts, byteOnly := splitForBudgets(prefix, body)
			if len(parts) <= byteOnly {
				t.Errorf("%d parts, byte budget alone gives %d: the split is not token-driven", len(parts), byteOnly)
			}
			for i, part := range parts {
				text := prefix + "\n\n" + part
				if n := estimateTokens(text); n > maxFileTokens {
					t.Errorf("part %d is %d tokens", i+1, n)
				}
				if len(text) > maxFileBytes {
					t.Errorf("part %d is %d bytes", i+1, len(text))
				}
				if strings.TrimSpace(part) == "" || !strings.HasPrefix(text, prefix) {
					t.Errorf("part %d is empty or lost the prefix", i+1)
				}
				if c := strings.Count(part, "```"); c%2 != 0 {
					t.Errorf("part %d has %d fence markers:\n%s", i+1, c, part)
				}
			}
		})
	}
}

func TestEstimateTokens(t *testing.T) {
	prose := strings.Repeat("The ledger records every transaction. ", 10) // 380 chars
	code := "```go\n" + strings.Repeat("x := foo(bar)\n", 10) + "```"
	for _, tc := range []struct {
		name     string
		text     string
		min, max int
	}{
		{"empty", "", 1, 1},
		{"prose at 4 chars per token", prose, 95, 100},
		{"code fence at 2 chars per token", code, 75, 90},
		{"symbol-heavy line outside a fence", strings.Repeat(`{"a":[1,2]}`, 10), 55, 60},
		{"non-ASCII counts one token per rune", strings.Repeat("é", 100), 100, 102},
	} {
		if got := estimateTokens(tc.text); got < tc.min || got > tc.max {
			t.Errorf("%s: %d tokens, want %d..%d", tc.name, got, tc.min, tc.max)
		}
	}
	if a, b := estimateTokens(code), estimateTokens(strings.ReplaceAll(code, "```", "")); a <= b {
		t.Errorf("fenced text (%d) must cost at least as much as the same text unfenced (%d)", a, b)
	}
}

func TestSplitForBudgetsHugeCodeLine(t *testing.T) {
	prefix := "Page > Section\nApplies to: Stack v4.0 and later"
	for name, line := range map[string]string{
		"with spaces": strings.TrimSpace(strings.Repeat("alpha(beta, gamma) ", 120)),
		"no spaces":   strings.Repeat("a1{}", 700),
		"multibyte":   strings.Repeat("é", 900),
	} {
		t.Run(name, func(t *testing.T) {
			parts, _ := splitForBudgets(prefix, "```text\n"+line+"\n```")
			if len(parts) < 2 {
				t.Fatalf("one huge line stayed in %d part", len(parts))
			}
			var joined strings.Builder
			for i, p := range parts {
				text := prefix + "\n\n" + p
				if n := estimateTokens(text); n > maxFileTokens || len(text) > maxFileBytes {
					t.Errorf("part %d: %d tokens, %d bytes", i+1, n, len(text))
				}
				if !strings.HasPrefix(p, "```text\n") || !strings.HasSuffix(p, "\n```") || !utf8.ValidString(p) {
					t.Errorf("part %d is not a closed text fence on valid UTF-8: %.40q", i+1, p)
				}
				joined.WriteString(strings.NewReplacer(" ", "", "\n", "").Replace(strings.TrimSuffix(strings.TrimPrefix(p, "```text\n"), "\n```")))
			}
			if joined.String() != strings.ReplaceAll(line, " ", "") {
				t.Error("pieces do not add up to the original line")
			}
		})
	}
}

func TestSplitForBudgetsKeepsSmallFilesUnchanged(t *testing.T) {
	prefix := "A > B\nApplies to: Stack v4.0 and later"
	for name, body := range map[string]string{
		"prose":      strings.Repeat("Short prose. ", 20),
		"small code": "```bash\nls -la\n```",
		"fits both":  strings.Repeat("word ", 200), // ~1000 bytes, ~250 tokens
	} {
		parts, byteOnly := splitForBudgets(prefix, body)
		if len(parts) != 1 || parts[0] != body || byteOnly != 1 {
			t.Errorf("%s: got %d parts (byte-only %d), want the body untouched", name, len(parts), byteOnly)
		}
	}
}

func TestBuildReportsTokenResplitsAndRejectsOverBudgetFiles(t *testing.T) {
	var sb strings.Builder
	sb.WriteString("# Page\n\n## Code\n\n```json\n")
	for range 60 {
		sb.WriteString(`{"a":[1,2,3]}` + "\n")
	}
	sb.WriteString("```\n")
	p := Build([]SourceFile{{Rel: "apps/docs/content/pages/ledger/code.mdx", Content: []byte(sb.String())}}, Options{Table: mustTable(t), Dates: fakeDates{}})
	if len(p.Errors) != 0 {
		t.Fatalf("errors: %v", p.Errors)
	}
	r := p.Report
	if r.TokenResplitChunks == 0 || r.TokenExtraParts == 0 || r.MaxFileTokens == 0 || r.MaxFileTokens > maxFileTokens || r.MaxFileTokensKey == "" {
		t.Errorf("token report not filled: %+v", r)
	}
	if errs := ValidateFileTokens(strings.Repeat("{}[]", 300)); len(errs) != 1 {
		t.Errorf("an over-budget body must fail validation: %v", errs)
	}
	if errs := ValidateFileTokens("short"); len(errs) != 0 {
		t.Errorf("a small body must pass: %v", errs)
	}
}
