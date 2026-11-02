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
        { version: "2.1.0" }, // no new interpreter
        { version: "2.2.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.11" } } },
        { version: "2.3.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.20" } } },
        { version: "2.3.1", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.20" } } },
        { version: "2.4.0", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.25" } } },
        { version: "2.4.5", requires: { "numscript-interpreter": { min: "0.0.10", below: "0.0.25" } } },
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
	if len(tab.ledger) != 9 {
		t.Errorf("ledger releases = %d, want 9", len(tab.ledger))
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
		{name: "stack empty range", component: "stack", minRaw: "v3.2", below: "v3.2", min: 302, bel: 302, empty: true},

		// Range releases: ledger is v3.0 [2.2.0,2.3.0), v3.1 [2.3.0,2.4.0), v3.2 [2.4.11,2.5.0), v4.0 point 3.0.0.
		{name: "regression: partial overlap with v3.1 is kept", component: "ledger", minRaw: "2.3.1", below: "2.4.0", min: 301, bel: 302},
		{name: "range equal to a release range", component: "ledger", minRaw: "2.3.0", below: "2.4.0", min: 301, bel: 302},
		{name: "range inside a release range", component: "ledger", minRaw: "2.2.5", below: "2.2.9", min: 300, bel: 301},
		{name: "range spanning two releases", component: "ledger", minRaw: "2.2.5", below: "2.4.12", min: 300, bel: 400},
		{name: "min at the exclusive end of a release does not overlap it", component: "ledger", minRaw: "2.4.0", min: 302, bel: NoUpperBound},
		{name: "below at the start of a release does not overlap it", component: "ledger", below: "2.3.0", bel: 301},
		{name: "below at the very first lower bound overlaps nothing", component: "ledger", below: "2.2.0", empty: true},
		{name: "range between two releases (2.4.0 to 2.4.10 never shipped)", component: "ledger", minRaw: "2.4.0", below: "2.4.11", empty: true},
		{name: "ledger [2.0.0, 2.2.0): all before v3.0", component: "ledger", minRaw: "2.0.0", below: "2.2.0", empty: true},
		{name: "min only inside v3.2", component: "ledger", minRaw: "2.4.11", min: 302, bel: NoUpperBound},
		{name: "ledger 3 only in v4", component: "ledger", minRaw: "3.0.0", min: 400, bel: NoUpperBound},
		{name: "below only on a point release", component: "ledger", below: "3.0.0", bel: 400},
		{name: "below only never reached", component: "ledger", below: "9.0.0", bel: NoUpperBound},
		{name: "min above every release is widened", component: "ledger", minRaw: "3.0.1", bel: NoUpperBound, widenedMin: true},

		// Point releases: payments is 3.0.0, 3.0.18, 3.4.0, then leaves the Stack at v4.0.
		{name: "point: min on a point", component: "payments", minRaw: "3.0.18", min: 301, bel: 400, capped: true},
		{name: "point: [min, below) containing one point", component: "payments", minRaw: "3.0.1", below: "3.1.0", min: 301, bel: 302},
		{name: "point: below equal to the point excludes it", component: "payments", minRaw: "3.0.0", below: "3.0.18", min: 300, bel: 301},
		{name: "point: between two points", component: "payments", minRaw: "3.0.1", below: "3.0.18", empty: true},
		{name: "point: [3.2.0, 3.3.0) (coinbaseprime, payments.capabilities) is between 3.0.18 and 3.4.0", component: "payments", minRaw: "3.2.0", below: "3.3.0", empty: true},
		{name: "point: min equal to the last point", component: "payments", minRaw: "3.4.0", min: 302, bel: 400, capped: true},
		{name: "point: above every release is widened, still capped", component: "payments", minRaw: "3.4.1", bel: 400, widenedMin: true, capped: true},
		{name: "point: below only", component: "payments", below: "3.1.0", bel: 302},
		{name: "point: below only is kept under the cap", component: "payments", below: "3.0.18", bel: 301},

		// Components leaving the Stack.
		{name: "leaves the Stack at v4.0", component: "payments", bel: 400, capped: true},
		{name: "upper bound beyond every release is capped", component: "payments", below: "9.0.0", bel: 400, capped: true},
		{name: "empty [m, m)", component: "orchestration", minRaw: "2.6.0", below: "2.6.0", empty: true},
		{name: "flows alias", component: "flows", minRaw: "2.6.0", min: 302, bel: 400, capped: true},

		// numscript-interpreter: interpreter range -> Ledger entries -> Ledger span -> Stack releases.
		{name: "cascade min", component: "numscript-interpreter", minRaw: "0.0.15", min: 301, bel: NoUpperBound},
		{name: "cascade first interpreter", component: "numscript-interpreter", minRaw: "0.0.10", min: 300, bel: NoUpperBound},
		{name: "cascade min at a ledger cap does not overlap that entry", component: "numscript-interpreter", minRaw: "0.0.20", below: "0.0.25", min: 302, bel: NoUpperBound},
		{name: "cascade below only: every entry starts at 0.0.10, so all overlap", component: "numscript-interpreter", below: "0.0.20", bel: NoUpperBound},
		{name: "cascade below the first interpreter", component: "numscript-interpreter", below: "0.0.10", empty: true},
		{name: "cascade min beyond ledgers", component: "numscript-interpreter", minRaw: "0.0.30", bel: NoUpperBound, widenedMin: true},

		{name: "unknown component", component: "membership", minRaw: "v2.0.0", bel: NoUpperBound, widenedMin: true},
		{name: "unknown component upper", component: "membership", below: "v2.0.0", bel: NoUpperBound, widenedBelow: true},
		{name: "unknown component without bounds", component: "membership", bel: NoUpperBound},
		{name: "unparsable version", component: "fctl", minRaw: "fctl-2.0", bel: NoUpperBound, widenedMin: true},
		{name: "unparsable upper bound widened, min kept", component: "ledger", minRaw: "2.4.11", below: "next", min: 302, bel: NoUpperBound, widenedBelow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := tab.Convert(tc.component, tc.minRaw, tc.below)
			if c.Empty {
				c.Min, c.Below = tc.min, tc.bel // values are meaningless when excluded
			}
			if c.Min != tc.min || c.Below != tc.bel || c.WidenedMin != tc.widenedMin || c.WidenedBelow != tc.widenedBelow || c.Empty != tc.empty || c.Capped != tc.capped {
				t.Errorf("Convert(%s, %q, %q) = %+v, want min %d below %d widened %v/%v empty %v capped %v",
					tc.component, tc.minRaw, tc.below, c, tc.min, tc.bel, tc.widenedMin, tc.widenedBelow, tc.empty, tc.capped)
			}
		})
	}
}

// The cascade with Ledger spans that end, and the non-adjacent cases.
func TestConvertCustomTables(t *testing.T) {
	cascade := `{ id: "stack", releases: [
	  { version: "v1.0", requires: { ledger: { min: "1.0.0", below: "2.0.0" } } },
	  { version: "v2.0", requires: { ledger: { min: "2.0.0", below: "3.0.0" } } },
	  { version: "v3.0", requires: { ledger: { min: "3.0.0", below: "4.0.0" } } },
	]},
	{ id: "ledger", releases: [
	  { version: "1.0.0", requires: { "numscript-interpreter": { min: "0.1.0", below: "0.2.0" } } },
	  { version: "2.0.0", requires: { "numscript-interpreter": { min: "0.2.0", below: "0.3.0" } } },
	  { version: "3.0.0", requires: { "numscript-interpreter": { min: "0.3.0", below: "0.4.0" } } },
	]}`
	gap := `{ id: "stack", releases: [
	  { version: "v3.0", requires: { x: "1.0.0", y: "1.0.0", z: "1.0.0" } },
	  { version: "v3.1", requires: { x: "5.0.0", y: "1.0.0" } },
	  { version: "v3.2", requires: { x: "1.5.0", z: "1.0.0" } },
	  { version: "v3.3", requires: { w: "1.0.0" } },
	  { version: "v3.4", requires: { x: "1.2.0" } },
	]}`
	for _, tc := range []struct {
		name, schema, component, minRaw, below string
		min, bel                               int
		empty, gap                             bool
	}{
		{name: "interpreter range maps to one Ledger span", schema: cascade, component: "numscript-interpreter", minRaw: "0.2.0", below: "0.3.0", min: 200, bel: 300},
		{name: "interpreter range across two Ledger entries", schema: cascade, component: "numscript-interpreter", minRaw: "0.1.5", below: "0.2.5", min: 100, bel: 300},
		{name: "interpreter range of the last entry has no end", schema: cascade, component: "numscript-interpreter", minRaw: "0.3.5", min: 300, bel: NoUpperBound},
		{name: "interpreter below at an entry start leaves it out", schema: cascade, component: "numscript-interpreter", below: "0.2.0", bel: 200},
		// x: 1.0.0 (v3.0), 5.0.0 (v3.1), 1.5.0 (v3.2), absent (v3.3), 1.2.0 (v3.4).
		// z: listed in v3.0 and v3.2, absent from v3.1. y: v3.0 and v3.1 only.
		{name: "overlap with v3.0 and v3.2 but not v3.1: superset to the cap, with a warning", schema: gap, component: "x", minRaw: "1.0.0", below: "2.0.0", min: 300, bel: 303, gap: true},
		{name: "relisted after leaving: the cap wins, with a warning", schema: gap, component: "z", minRaw: "1.0.0", below: "2.0.0", min: 300, bel: 301, gap: true},
		{name: "range only matched after the component left and came back: first match wins, no cap", schema: gap, component: "x", minRaw: "1.2.0", below: "1.3.0", min: 304, bel: NoUpperBound},
		{name: "adjacent releases: no warning", schema: gap, component: "y", minRaw: "1.0.0", below: "2.0.0", min: 300, bel: 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tab, err := ParseSchema(tc.schema)
			if err != nil {
				t.Fatal(err)
			}
			c := tab.Convert(tc.component, tc.minRaw, tc.below)
			if c.Empty != tc.empty || c.NonContiguous != tc.gap || (!c.Empty && (c.Min != tc.min || c.Below != tc.bel)) {
				t.Errorf("Convert(%s, %q, %q) = %+v, want [%d, %d) empty %v gap %v", tc.component, tc.minRaw, tc.below, c, tc.min, tc.bel, tc.empty, tc.gap)
			}
		})
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
