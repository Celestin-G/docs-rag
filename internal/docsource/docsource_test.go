package docsource

import "testing"

// IsDocFile decides what reaches the index. Anything it lets through that is
// not published documentation ends up competing with the real pages in
// retrieval — and, in the case of the connector template, inventing a stack
// version that does not exist.
func TestIsDocFile(t *testing.T) {
	for path, want := range map[string]bool{
		"apps/docs/content/pages/modules/ledger/quick-start.mdx":  true,
		"apps/docs/content/fragments/ledger/api-routes.mdx":       true,
		"apps/docs/content/data/generated/api.operations.json":    true,
		"packages/numscript-engine/README.md":                     true,
		"apps/docs/content/templates/connector-page.template.mdx": false,
		"apps/docs/content/AUDIT.payments-v3.3.md":                false,
		"apps/docs/content/schemas-library/x.md":                  false,
		"apps/docs/content/scripts/build.md":                      false,
		"apps/docs/content/data/other.json":                       false,
		"apps/docs/package.json":                                  false,
		"README.md":                                               false,
		"CLAUDE.md":                                               false,
	} {
		if got := IsDocFile(path); got != want {
			t.Errorf("IsDocFile(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestKind(t *testing.T) {
	for _, tc := range []struct{ rel, want string }{
		{"apps/docs/content/pages/a.mdx", KindPage},
		{"apps/docs/content/fragments/a.mdx", KindFragment},
		{"apps/docs/content/data/generated/a.json", KindGenerated},
		{"packages/numscript-engine/README.md", KindNumscriptEngine},
		{"README.md", ""},
	} {
		if got := Kind(tc.rel); got != tc.want {
			t.Errorf("Kind(%q) = %q, want %q", tc.rel, got, tc.want)
		}
	}
}
