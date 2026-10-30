package chunker

import "testing"

// The real frontmatter of modules/numscript/reference/oneof.mdx. Its version
// bound is expressed against the interpreter, not the stack — which is the case
// that used to be read as a stack version and silently stop filtering.
const oneofFrontmatter = `---
path: /modules/numscript/reference/oneof
title: "oneof"
section: Modules
order: 41
component: numscript-interpreter
version: { min: "0.0.15" }
visibility: { numscript: "new" }
badge: experimental
---

Some body text.
`

func TestParseMetaReadsComponentScopedVersion(t *testing.T) {
	m := parseMeta("apps/docs/content/pages/modules/numscript/reference/oneof.mdx", []byte(oneofFrontmatter))

	if m.component != "numscript-interpreter" {
		t.Errorf("component = %q, want %q", m.component, "numscript-interpreter")
	}
	if m.badge != "experimental" {
		t.Errorf("badge = %q, want %q", m.badge, "experimental")
	}
	if m.pageTitle != "oneof" {
		t.Errorf("pageTitle = %q, want %q", m.pageTitle, "oneof")
	}
	// "0.0.15" on the interpreter's own scale collapses to 0 under the stack
	// encoding. That is acceptable *because* the component is recorded: the
	// search filter skips version bounds that are not stack-scoped, so the page
	// is no longer filtered by a number that means nothing for it.
	if m.versionNum != 0 {
		t.Errorf("versionNum = %d, want 0", m.versionNum)
	}
}

func TestParseMetaDefaultsToStack(t *testing.T) {
	fm := `---
title: "Selecting an Interpreter"
version: { min: "v3.1" }
---

Body.
`
	m := parseMeta("apps/docs/content/pages/modules/numscript/interpreter.mdx", []byte(fm))
	if m.component != "stack" {
		t.Errorf("component = %q, want %q for a page that declares none", m.component, "stack")
	}
	if m.versionNum != 31 {
		t.Errorf("versionNum = %d, want 31", m.versionNum)
	}
}

func TestParseMetaDeploymentFromPath(t *testing.T) {
	for path, want := range map[string]string{
		"apps/docs/content/pages/deploy/cloud/regions.mdx":       "cloud",
		"apps/docs/content/pages/deploy/self-hosted/helm.mdx":    "self-hosted",
		"apps/docs/content/pages/modules/ledger/quick-start.mdx": "both",
	} {
		if got := parseMeta(path, nil).deployment; got != want {
			t.Errorf("deployment for %s = %q, want %q", path, got, want)
		}
	}
}

// The raw frontmatter strings and heading fields are what the knowledge-base
// publisher builds its metadata from; the compact int16 fields cannot carry them.
func TestParseFileKeepsRawMetadata(t *testing.T) {
	src := `---
path: /modules/ledger/accounts
title: "Accounts"
component: ledger
version: { min: "2.4.0", below: "3.0.0" }
---

Intro text.

## Model

Model text.

### Balances ##

Balance text.
`
	chunks := ParseFile("apps/docs/content/pages/modules/ledger/accounts.mdx", []byte(src))
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	for i, want := range []struct {
		leaf, breadcrumb, body string
	}{
		{"", "Accounts", "Intro text."},
		{"Model", "Accounts > Model", "Model text."},
		{"Balances ##", "Accounts > Model > Balances ##", "Balance text."},
	} {
		c := chunks[i]
		if c.Leaf != want.leaf || c.Breadcrumb != want.breadcrumb || c.Body != want.body || c.SectionSeq != i {
			t.Errorf("chunk %d = {%q %q %q seq %d}, want %+v", i, c.Leaf, c.Breadcrumb, c.Body, c.SectionSeq, want)
		}
		if c.Content != c.Breadcrumb+"\n\n"+c.Body {
			t.Errorf("chunk %d Content %q is not breadcrumb + body", i, c.Content)
		}
		if c.PagePath != "/modules/ledger/accounts" || c.Title != "Accounts" ||
			c.VersionMinRaw != "2.4.0" || c.VersionBelowRaw != "3.0.0" || c.VersionComponent != "ledger" {
			t.Errorf("chunk %d metadata = %+v", i, c)
		}
	}
}

func TestParseGeneratedJSONKeepsRawRanges(t *testing.T) {
	src := `{"ledger.events": {"__component": "ledger", "__versions": [
	  {"range": {"min": "2.3.0", "below": "2.4.0"}, "data": {"A": "same"}},
	  {"range": {"min": "2.4.0", "below": "3.0.0"}, "data": {"A": "same"}}]}}`
	chunks := ParseGeneratedJSON("x.json", []byte(src))
	if len(chunks) != 1 || chunks[0].VersionMinRaw != "2.3.0" || chunks[0].VersionBelowRaw != "3.0.0" {
		t.Fatalf("got %+v, want one chunk spanning 2.3.0 to 3.0.0", chunks)
	}
}
