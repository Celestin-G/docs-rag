package kbpublish

import (
	"strings"
	"testing"
)

func validAttrs() Attrs {
	return Attrs{
		"doc_type": "doc", "product": "ledger", "version_min": 301, "version_below": 99999,
		"version_component": "ledger", "deployment": "any", "license": "ee", "client_id": "none", "language": "en",
		"source_url": "https://docs.formance.com/modules/ledger/accounts#balances", "updated_at": "2026-10-30",
	}
}

func TestAttrsValidate(t *testing.T) {
	if errs := validAttrs().Validate(); len(errs) != 0 {
		t.Fatalf("valid sidecar rejected: %v", errs)
	}
	for _, tc := range []struct {
		name   string
		mutate func(Attrs)
		want   string // substring of an expected error
	}{
		{"unknown key", func(a Attrs) { a["extra"] = "x" }, `unknown attribute "extra"`},
		{"missing key", func(a Attrs) { delete(a, "language") }, `missing attribute "language"`},
		{"wrong doc_type", func(a Attrs) { a["doc_type"] = "ticket" }, `"doc_type"`},
		{"unknown product", func(a Attrs) { a["product"] = "billing" }, `"product"`},
		{"webhooks product is allowed", func(a Attrs) { a["product"] = "webhooks" }, ""},
		{"bad deployment", func(a Attrs) { a["deployment"] = "both" }, `"deployment"`},
		{"bad license", func(a Attrs) { a["license"] = "paid" }, `"license"`},
		{"client_id must be none", func(a Attrs) { a["client_id"] = "acme" }, `"client_id"`},
		{"language", func(a Attrs) { a["language"] = "fr" }, `"language"`},
		{"legacy version key is dropped", func(a Attrs) { a["version"] = "any" }, `unknown attribute "version"`},
		{"version_min as string", func(a Attrs) { a["version_min"] = "301" }, "must be an integer"},
		{"version_min fractional", func(a Attrs) { a["version_min"] = 3.5 }, "must be an integer"},
		{"min not below", func(a Attrs) { a["version_min"] = 400; a["version_below"] = 400 }, "not below"},
		{"below too large", func(a Attrs) { a["version_below"] = 100000 }, "above 99999"},
		{"min negative", func(a Attrs) { a["version_min"] = -1 }, "negative"},
		{"http url", func(a Attrs) { a["source_url"] = "http://docs.formance.com/x" }, "not https"},
		{"query in url", func(a Attrs) { a["source_url"] = "https://docs.formance.com/x?y=1" }, "contains '?'"},
		{"long url", func(a Attrs) { a["source_url"] = "https://docs.formance.com/" + strings.Repeat("a", 200) }, "limit is 200"},
		{"fragment is allowed", func(a Attrs) { a["source_url"] = "https://docs.formance.com/x#y" }, ""},
		{"updated_at format", func(a Attrs) { a["updated_at"] = "30/10/2026" }, "YYYY-MM-DD"},
		{"updated_at not a date", func(a Attrs) { a["updated_at"] = "2026-13-45" }, "not a date"},
		{"too many bytes", func(a Attrs) {
			a["version_component"] = strings.Repeat("a", 64)
			a["source_url"] = "https://docs.formance.com/" + strings.Repeat("b", 170)
			for i := range 20 {
				a[strings.Repeat("k", i+1)] = strings.Repeat("v", 20)
			}
		}, "limit is 900"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := validAttrs()
			tc.mutate(a)
			errs := a.Validate()
			if tc.want == "" {
				if len(errs) != 0 {
					t.Fatalf("unexpected errors: %v", errs)
				}
				return
			}
			if !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Errorf("errors %v lack %q", errs, tc.want)
			}
		})
	}
}

func TestAttrsValidateCollectsEveryError(t *testing.T) {
	a := validAttrs()
	a["product"], a["language"], a["updated_at"] = "x", "x", "x"
	if n := len(a.Validate()); n != 3 {
		t.Errorf("got %d errors, want 3", n)
	}
}

func TestValidateKey(t *testing.T) {
	for _, tc := range []struct {
		key string
		ok  bool
	}{
		{"docs/ledger/a/b.md", true},
		{"docs/ledger/_data/api.ops/x-1.md", true},
		{"other/a.md", false},
		{"docs/Ledger/a.md", false},
		{"docs/ledger/a b.md", false},
		{"docs/ledger/é.md", false},
		{"docs/ledger/a.txt", false},
		{"docs/../a.md", false},
		{"docs//a.md", false},
		{"docsx/a.md", false},
		{"docs.md", false},
		{"docs/./a.md", false},
		{"/docs/a.md", false},
		{"docs/" + strings.Repeat("a", 1100) + ".md", false},
	} {
		if got := len(ValidateKey(tc.key)) == 0; got != tc.ok {
			t.Errorf("ValidateKey(%.40q) ok = %v, want %v", tc.key, got, tc.ok)
		}
	}
}
