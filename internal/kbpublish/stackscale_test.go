package kbpublish

import (
	"strings"
	"testing"
)

// A copy of the relevant part of apps/docs/content/schema.ts (formancehq/docs),
// comments included: the parser must cope with them.
const schemaFixture = `
export const formanceSchema: Schema = {
  components: [
    { id: "stack", name: "Stack", releases: [
      { version: "v3.0", requires: {
        ledger: { min: "2.2.0", below: "2.3.0" },
        payments: "3.0.0",
        fctl: { min: "fctl-2.0" },
      }},
      { version: "v3.1", requires: {
        ledger: { min: "2.3.0", below: "2.4.0" },
        payments: "3.0.18",
        fctl: { min: "fctl-2.1" },
      }},
      // v3.2 includes the component versions prepared for the cancelled v3.3.
      { version: "v3.2", requires: {
        ledger: { min: "2.4.11", below: "2.5.0" },
        payments: "3.4.0", // a "quoted" // comment
        orchestration: "2.6.0",
        fctl: { min: "fctl-2.2" },
      }},
      { version: "v4.0", requires: {
        ledger: "3.0.0",
        fctl: { min: "fctl-2.2" },
      }},
    ]},
    { id: "ledger", name: "Ledger",
      releases: [
        { version: "2.0.0" }, // no new interpreter
        { version: "2.2.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.11" } } },
        { version: "2.3.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.20" } } },
        { version: "2.4.11", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.25" } } },
        { version: "3.0.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.26" } } },
      ],
    },
    { id: "payments", name: "Payments", releases: [{ version: "3.0.0" }] },
  ],
};
`

func mustTable(t *testing.T) *StackTable {
	t.Helper()
	tab, err := ParseSchema(schemaFixture)
	if err != nil {
		t.Fatal(err)
	}
	return tab
}

func TestParseSchema(t *testing.T) {
	tab := mustTable(t)
	if got := strings.Join(tab.Releases(), ","); got != "v3.0,v3.1,v3.2,v4.0" {
		t.Errorf("releases = %s", got)
	}
	if len(tab.ledger) != 5 {
		t.Errorf("ledger releases = %d, want 5", len(tab.ledger))
	}
	if req := tab.stack[2].requires["payments"]; req.min != "3.4.0" || req.below != "" {
		t.Errorf("v3.2 payments = %+v", req)
	}
	if req := tab.stack[0].requires["ledger"]; req.min != "2.2.0" || req.below != "2.3.0" {
		t.Errorf("v3.0 ledger = %+v", req)
	}

	for name, src := range map[string]string{
		"no stack block":     `{ id: "ledger", releases: [{ version: "2.0.0" }] }`,
		"no releases":        `{ id: "stack", name: "Stack" }, { id: "ledger", releases: [] }`,
		"empty release list": `{ id: "stack", releases: [] }`,
		"bad version label":  `{ id: "stack", releases: [{ version: "latest" }] }`,
		"entry no version":   `{ id: "stack", releases: [{ requires: {} }] }`,
		"unbalanced":         `{ id: "stack", releases: [{ version: "v3.0"`,
	} {
		if _, err := ParseSchema(src); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestConvert(t *testing.T) {
	tab := mustTable(t)
	for _, tc := range []struct {
		name                     string
		component, minRaw, below string
		min, bel                 int
		widenedMin, widenedBelow bool
		empty, capped            bool
	}{
		{name: "no bounds", component: "ledger", bel: NoUpperBound},
		{name: "stack direct", component: "stack", minRaw: "v3.2", min: 302, bel: NoUpperBound},
		{name: "stack from patch version", component: "stack", minRaw: "3.1.4", min: 301, bel: NoUpperBound},
		{name: "stack bound beyond the table", component: "stack", below: "v5.0", bel: 500},
		{name: "ledger lower bound", component: "ledger", minRaw: "2.4.0", min: 302, bel: NoUpperBound},
		{name: "ledger 3 only in v4", component: "ledger", minRaw: "3.0.0", min: 400, bel: NoUpperBound},
		{name: "ledger upper bound", component: "ledger", below: "3.0.0", bel: 400},
		{name: "ledger upper bound never reached", component: "ledger", below: "9.0.0", bel: NoUpperBound},
		{name: "payments point version", component: "payments", minRaw: "3.0.18", min: 301, bel: 400, capped: true},
		{name: "payments above every release", component: "payments", minRaw: "3.5.0", bel: 400, widenedMin: true, capped: true},
		{name: "payments upper bound", component: "payments", below: "3.1.0", bel: 302},
		{name: "empty range is not widened", component: "payments", minRaw: "3.2.0", below: "3.3.0", min: 302, bel: 302, empty: true},
		{name: "payments leaves the Stack at v4.0", component: "payments", bel: 400, capped: true},
		{name: "upper bound beyond every release is capped", component: "payments", below: "9.0.0", bel: 400, capped: true},
		{name: "upper bound below the cap is kept", component: "payments", below: "3.0.18", bel: 301},
		{name: "lower bound at the cap is empty", component: "orchestration", minRaw: "2.6.0", below: "2.6.0", min: 302, bel: 302, empty: true},
		{name: "flows alias", component: "flows", minRaw: "2.6.0", min: 302, bel: 400, capped: true},
		{name: "cascade lower", component: "numscript-interpreter", minRaw: "0.0.15", min: 301, bel: NoUpperBound},
		{name: "cascade first interpreter", component: "numscript-interpreter", minRaw: "0.0.10", min: 300, bel: NoUpperBound},
		{name: "cascade upper", component: "numscript-interpreter", below: "0.0.20", bel: 302},
		{name: "cascade lower beyond ledgers", component: "numscript-interpreter", minRaw: "0.0.30", bel: NoUpperBound, widenedMin: true},
		{name: "unknown component", component: "membership", minRaw: "v2.0.0", bel: NoUpperBound, widenedMin: true},
		{name: "unknown component upper", component: "membership", below: "v2.0.0", bel: NoUpperBound, widenedBelow: true},
		{name: "unparsable version", component: "fctl", minRaw: "fctl-2.0", bel: NoUpperBound, widenedMin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tab.Convert(tc.component, tc.minRaw, tc.below)
			if c.Min != tc.min || c.Below != tc.bel || c.WidenedMin != tc.widenedMin || c.WidenedBelow != tc.widenedBelow || c.Empty != tc.empty || c.Capped != tc.capped {
				t.Errorf("Convert(%s, %q, %q) = %+v, want min %d below %d widened %v/%v empty %v capped %v",
					tc.component, tc.minRaw, tc.below, c, tc.min, tc.bel, tc.widenedMin, tc.widenedBelow, tc.empty, tc.capped)
			}
		})
	}
}

// A component listed, then absent, then listed again: the cap is the first
// release without it, and a lower bound shipped only after that is empty.
func TestConvertLowerBoundAfterCap(t *testing.T) {
	tab, err := ParseSchema(`{ id: "stack", releases: [
	  { version: "v3.0", requires: { ledger: "2.0.0", x: "1.0.0" } },
	  { version: "v3.1", requires: { ledger: "2.0.0" } },
	  { version: "v3.2", requires: { ledger: "2.0.0", x: "2.0.0" } },
	]}`)
	if err != nil {
		t.Fatal(err)
	}
	if c := tab.Convert("x", "2.0.0", ""); !c.Empty || c.Min != 302 || c.Below != 301 {
		t.Errorf("lower bound after the cap = %+v, want empty [302, 301)", c)
	}
	if c := tab.Convert("x", "1.0.0", ""); c.Empty || c.Min != 300 || c.Below != 301 {
		t.Errorf("lower bound before the cap = %+v, want [300, 301)", c)
	}
	if c := tab.Convert("numscript-interpreter", "", ""); c.Capped {
		t.Errorf("the interpreter follows ledger, which never leaves: %+v", c)
	}
}

func TestNotes(t *testing.T) {
	tab := mustTable(t)
	for _, tc := range []struct {
		name         string
		comp, lo, hi string
		aud          Audience
		note, header string
	}{
		{"from", "stack", "v3.2", "", Audience{}, "(Applies from Stack v3.2)", "Applies to: Stack v3.2 and later"},
		{"before", "stack", "", "v3.2", Audience{}, "(Applies before Stack v3.2)", "Applies to: before Stack v3.2"},
		{"range", "stack", "v3.1", "v3.2", Audience{}, "(Applies from Stack v3.1, before v3.2)", "Applies to: Stack v3.1 up to before v3.2"},
		{"component scale kept in header", "ledger", "3.0.0", "", Audience{}, "(Applies from Stack v4.0)", "Applies to: Stack v4.0 and later (ledger 3.0.0 and later)"},
		{"widened keeps component", "membership", "v2.0.0", "", Audience{}, "(Applies from membership v2.0.0)", "Applies to: membership v2.0.0 and later"},
		{"audience", "stack", "", "", Audience{License: "ee", Deployment: "self-hosted", Experiment: true}, "(Applies to Enterprise Edition, self-hosted)", "Applies to: Enterprise Edition; self-hosted; experimental feature"},
		{"version and audience", "stack", "v3.1", "", Audience{Numscript: "new"}, "(Applies from Stack v3.1, to new Numscript interpreter)", "Applies to: Stack v3.1 and later; new Numscript interpreter"},
		{"component leaving the Stack is capped", "payments", "", "", Audience{}, "(Applies before Stack v4.0)", "Applies to: before Stack v4.0"},
		{"component lower bound and cap", "payments", "3.0.18", "", Audience{}, "(Applies from Stack v3.1, before v4.0)", "Applies to: Stack v3.1 up to before v4.0 (payments 3.0.18 and later)"},
		{"nothing", "stack", "", "", Audience{}, "", ""},
		{"lower bound at the first release restricts nothing", "stack", "v3.0", "", Audience{}, "", ""},
		{"first-release bound kept out of a range", "stack", "v3.0", "v3.2", Audience{}, "(Applies before Stack v3.2)", "Applies to: before Stack v3.2"},
		{"component bound at the first release", "ledger", "2.2.0", "", Audience{License: "ee"}, "(Applies to Enterprise Edition)", "Applies to: Enterprise Edition"},
		{"widened range names the component once", "membership", "v2.0.0", "v2.3.0", Audience{}, "(Applies from membership v2.0.0, before v2.3.0)", "Applies to: membership v2.0.0 up to before v2.3.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tab.Convert(tc.comp, tc.lo, tc.hi)
			if got := InlineNote(c, tc.aud); got != tc.note {
				t.Errorf("note = %q, want %q", got, tc.note)
			}
			if got := Header(c, tc.aud); got != tc.header {
				t.Errorf("header = %q, want %q", got, tc.header)
			}
		})
	}
}
