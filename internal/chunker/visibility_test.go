package chunker

import (
	"reflect"
	"testing"
)

func TestParseVisibility(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want map[string][]string
	}{
		"quoted value":   {`numscript: "new"`, map[string][]string{"numscript": {"new"}}},
		"single quotes":  {`numscript: 'new'`, map[string][]string{"numscript": {"new"}}},
		"bare word":      {`numscript: new`, map[string][]string{"numscript": {"new"}}},
		"list":           {`sdk: ["go", "python"]`, map[string][]string{"sdk": {"go", "python"}}},
		"two dimensions": {`numscript: "new", deployment: "self-hosted"`, map[string][]string{"numscript": {"new"}, "deployment": {"self-hosted"}}},
		"empty":          {``, nil},
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseVisibility(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseVisibility(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

// The real gate on experimental pages: oneof.mdx exists only when the new
// interpreter is selected. Missing this field is why the RAG served experimental
// features regardless of the reader's setup.
func TestParseMetaReadsVisibility(t *testing.T) {
	m := parseMeta("apps/docs/content/pages/modules/numscript/reference/oneof.mdx", []byte(oneofFrontmatter))
	want := map[string][]string{"numscript": {"new"}}
	if !reflect.DeepEqual(m.visibility, want) {
		t.Errorf("visibility = %#v, want %#v", m.visibility, want)
	}
}

func TestParseMetaNoVisibility(t *testing.T) {
	fm := "---\ntitle: \"Quick Start\"\n---\n\nBody.\n"
	if got := parseMeta("x.mdx", []byte(fm)).visibility; got != nil {
		t.Errorf("visibility = %#v, want nil for a page that declares none", got)
	}
}

// A page gated on a dimension must carry that gate into every chunk it produces,
// not just the first.
func TestParseFileCarriesVisibilityToChunks(t *testing.T) {
	src := oneofFrontmatter + "\n## First\n\nAlpha.\n\n## Second\n\nBeta.\n"
	chunks := ParseFile("apps/docs/content/pages/modules/numscript/reference/oneof.mdx", []byte(src))
	if len(chunks) < 2 {
		t.Fatalf("got %d chunks, want at least 2", len(chunks))
	}
	for i, c := range chunks {
		if !reflect.DeepEqual(c.Visibility, map[string][]string{"numscript": {"new"}}) {
			t.Errorf("chunk %d visibility = %#v, want the page's gate", i, c.Visibility)
		}
	}
}

// The deployment column was inferred from the file path, which only catches
// pages filed under deploy/. Self-hosted-only material lives all over the tree,
// and those pages reached Cloud readers unfiltered.
func TestVisibilityDeploymentOverridesThePath(t *testing.T) {
	for name, tc := range map[string]struct {
		path string
		vis  string
		want string
	}{
		"frontmatter states self-hosted outside deploy/": {
			"apps/docs/content/pages/operate/backups.mdx", `deployment: "self-hosted"`, "self-hosted"},
		"frontmatter states cloud": {
			"apps/docs/content/pages/operate/regions.mdx", `deployment: "cloud"`, "cloud"},
		"frontmatter agrees with the path": {
			"apps/docs/content/pages/deploy/self-hosted/demo.mdx", `deployment: "self-hosted"`, "self-hosted"},
		"both values listed is no constraint": {
			"apps/docs/content/pages/operate/x.mdx", `deployment: ["cloud", "self-hosted"]`, "both"},
		"another dimension leaves deployment alone": {
			"apps/docs/content/pages/operate/y.mdx", `numscript: "new"`, "both"},
	} {
		t.Run(name, func(t *testing.T) {
			fm := "---\ntitle: \"T\"\nvisibility: { " + tc.vis + " }\n---\n\nBody.\n"
			if got := parseMeta(tc.path, []byte(fm)).deployment; got != tc.want {
				t.Errorf("deployment = %q, want %q", got, tc.want)
			}
		})
	}
}

// The path stays authoritative when the frontmatter says nothing.
func TestPathStillSetsDeploymentWithoutVisibility(t *testing.T) {
	fm := "---\ntitle: \"T\"\n---\n\nBody.\n"
	if got := parseMeta("apps/docs/content/pages/deploy/cloud/start.mdx", []byte(fm)).deployment; got != "cloud" {
		t.Errorf("deployment = %q, want cloud", got)
	}
}
