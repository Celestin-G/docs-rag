package kbpublish

import (
	"strings"
	"testing"
)

func TestRewriteInline(t *testing.T) {
	tab := mustTable(t)
	for _, tc := range []struct {
		name, in  string
		want      []string // lines the output must contain, in order
		notWant   []string
		when, dc  int
		widened   int
		unhandled int
	}{
		{
			name: "from stack version", in: "<When component=\"stack\" minimum=\"v3.2\">\n\nText.\n\n</When>",
			want: []string{"(Applies from Stack v3.2)", "Text."}, when: 1,
		},
		{
			name: "range on a component", in: "<When component=\"payments\" minimum=\"3.0.18\" below=\"3.1.0\">\nText\n</When>",
			want: []string{"(Applies from Stack v3.1, before v3.2)"}, when: 1,
		},
		{
			name: "visibility only", in: "<When visibility={{ license: \"ee\" }}>\nEE text\n</When>",
			want: []string{"(Applies to Enterprise Edition)"}, when: 1,
		},
		{
			name: "deployment and list value ignored", in: "<When visibility={{ deployment: \"cloud\", sdk: [\"go\", \"java\"] }}>\nx\n</When>",
			want: []string{"(Applies to Cloud)"}, when: 1,
		},
		{
			name: "fallback attribute is not parsed as a bound",
			in:   "<When visibility={{ numscript: \"new\" }} component=\"numscript-interpreter\" minimum=\"0.0.15\" fallback={<DocCallout minimum=\"9.9.9\">no</DocCallout>}>\nok\n</When>",
			want: []string{"(Applies from Stack v3.1, to new Numscript interpreter)"}, when: 1,
		},
		{
			name: "unconvertible bound keeps component note and is widened",
			in:   "<When component=\"membership\" minimum=\"v2.0.0\">\nx\n</When>",
			want: []string{"(Applies from membership v2.0.0)"}, when: 1, widened: 1,
		},
		{
			name: "multi-line opening tag", in: "<When\n  component=\"stack\"\n  below=\"v4.0\">\nx\n</When>",
			want: []string{"(Applies before Stack v4.0)", "x"}, when: 1,
		},
		{
			name: "constrains nothing", in: "<When component=\"ledger\">\nx\n</When>",
			want: []string{"x"}, notWant: []string{"Applies"},
		},
		{
			name: "block with only a stripped component gets no note", in: "<When component=\"membership\" minimum=\"v2.0.0\">\n<DocAPI method=\"POST\" path=\"/x\"\n  body={{ a: 1 }} />\n</When>\n\nAfter.",
			want: []string{"After."}, notWant: []string{"Applies"},
		},
		{name: "doc case with only a stripped component gets no note", in: "<DocCase value=\"ee\">\n<DocAPI path=\"/x\" />\n</DocCase>\nAfter.", want: []string{"After."}, notWant: []string{"Enterprise"}},
		{name: "unclosed block keeps its note", in: "<When component=\"stack\" minimum=\"v3.2\">\n<DocAPI path=\"/x\" />", want: []string{"(Applies from Stack v3.2)"}, when: 1},
		{name: "doc case ee", in: "<DocCase value=\"ee\">\nx\n</DocCase>", want: []string{"(Enterprise Edition)"}, dc: 1},
		{name: "doc case oss", in: "<DocCase value=\"oss\">\nx\n</DocCase>", want: []string{"(Open source edition)"}, dc: 1},
		{name: "doc case other value untouched", in: "<DocCase value=\"go\">\nx", want: []string{"<DocCase value=\"go\">"}},
		{
			name:    "code fence and frontmatter untouched",
			in:      "---\ntitle: <When component=\"stack\" minimum=\"v3.2\">\n---\n```mdx\n<When component=\"stack\" minimum=\"v3.2\">\n```\n",
			notWant: []string{"Applies"},
		},
		{
			name: "content after the tag on the same line is reported", in: "<When component=\"stack\" minimum=\"v3.2\">inline</When>",
			want: []string{"<When component"}, unhandled: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, rep := RewriteInline("f.mdx", []byte(tc.in), tab)
			text := string(out)
			pos := 0
			for _, w := range tc.want {
				i := strings.Index(text[pos:], w)
				if i < 0 {
					t.Fatalf("output lacks %q after offset %d:\n%s", w, pos, text)
				}
				pos += i + len(w)
			}
			for _, w := range tc.notWant {
				if strings.Contains(text, w) {
					t.Errorf("output contains %q:\n%s", w, text)
				}
			}
			if rep.WhenNotes != tc.when || rep.DocCaseNotes != tc.dc || len(rep.Widened) != tc.widened || len(rep.Unhandled) != tc.unhandled {
				t.Errorf("report = %+v, want when %d docCase %d widened %d unhandled %d", rep, tc.when, tc.dc, tc.widened, tc.unhandled)
			}
		})
	}
}
