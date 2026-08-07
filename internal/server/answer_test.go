package server

import "testing"

func TestSourceURL(t *testing.T) {
	for _, tc := range []struct {
		name    string
		file    string
		section string
		want    string
	}{
		{
			name: "page",
			file: "apps/docs/content/pages/modules/ledger/quick-start.mdx",
			want: "https://docs.formance.com/modules/ledger/quick-start",
		},
		{
			name: "nested page",
			file: "apps/docs/content/pages/modules/ledger/working-with/ledger-schema.mdx",
			want: "https://docs.formance.com/modules/ledger/working-with/ledger-schema",
		},
		{
			name: "index page drops the trailing segment",
			file: "apps/docs/content/pages/modules/numscript/index.mdx",
			want: "https://docs.formance.com/modules/numscript",
		},
		{
			name: "markdown extension",
			file: "apps/docs/content/pages/getting-started/connect-app.md",
			want: "https://docs.formance.com/getting-started/connect-app",
		},
		{
			name: "fragment has no standalone page",
			file: "apps/docs/content/fragments/ledger/api-routes.mdx",
			want: "",
		},
		{
			name:    "heading becomes an anchor",
			file:    "apps/docs/content/pages/modules/ledger/quick-start.mdx",
			section: "Creating a transaction",
			want:    "https://docs.formance.com/modules/ledger/quick-start#creating-a-transaction",
		},
		{
			name:    "punctuation and code ticks are dropped, not hyphenated",
			file:    "apps/docs/content/pages/modules/numscript/reference/oneof.mdx",
			section: "Using `oneof` on the source side?",
			want:    "https://docs.formance.com/modules/numscript/reference/oneof#using-oneof-on-the-source-side",
		},
		{
			name:    "a generated dataset label is not a heading",
			file:    "apps/docs/content/pages/modules/ledger/quick-start.mdx",
			section: "api.operations \u203a v2CreateLedger",
			want:    "https://docs.formance.com/modules/ledger/quick-start",
		},
		{
			name: "unrelated path",
			file: "README.md",
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceURL(tc.file, tc.section); got != tc.want {
				t.Errorf("sourceURL(%q, %q) = %q, want %q", tc.file, tc.section, got, tc.want)
			}
		})
	}
}

func TestRenderSourcesEmpty(t *testing.T) {
	if got := renderSources(nil); got != "" {
		t.Errorf("renderSources(nil) = %q, want empty", got)
	}
}

func TestRenderSources(t *testing.T) {
	// Same page twice: numbering must follow position, not identity, because the
	// model cites [i+1] for the i-th excerpt it was given. With anchors the two
	// entries also resolve to different places on that page, which is the whole
	// reason not to deduplicate them.
	got := renderSources([]source{
		{File: "apps/docs/content/fragments/ledger/api-routes.mdx"},
		{File: "apps/docs/content/pages/modules/ledger/quick-start.mdx", Section: "Your First Transaction"},
		{File: "apps/docs/content/pages/modules/ledger/quick-start.mdx", Section: "Splitting Funds"},
	})

	want := "\n\n---\nSources:\n" +
		"[1] apps/docs/content/fragments/ledger/api-routes.mdx\n" +
		"[2] Your First Transaction — https://docs.formance.com/modules/ledger/quick-start#your-first-transaction\n" +
		"[3] Splitting Funds — https://docs.formance.com/modules/ledger/quick-start#splitting-funds\n"

	if got != want {
		t.Errorf("renderSources mismatch\ngot:\n%s\nwant:\n%s", got, want)
	}
}
