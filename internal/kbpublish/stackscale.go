// Package kbpublish turns the Formance docs into files for an Amazon Bedrock
// Knowledge Base: one markdown body and one metadata sidecar per chunk, the
// diff against what is already in the S3 bucket, and the upload and ingestion.
package kbpublish

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// NoUpperBound is the version_below of content with no upper bound.
const NoUpperBound = 99999

// A Stack release lists, per component, the versions it allows: a point version
// ("3.0.18", exactly that one) or a range ({min, below}, or {min} alone, open
// above). Component bounds in the docs (a page for
// payments 3.3.0, a block for ledger below 3.0.0) mean nothing to a customer who
// only knows their Stack version, so they are converted to the Stack scale with
// this table. The table is read from the docs repo's schema.ts at publish time
// rather than copied here, so it cannot drift.

type requirement struct {
	min, below string // below is empty for a point version and for a {min}-only range
	point      bool   // written as a string ("3.0.18"): exactly that version
}

type release struct {
	version  string // as written: "v3.1", "2.4.11"
	requires map[string]requirement
}

// StackTable converts component version bounds to the Stack scale (vX.Y is
// X*100+Y).
type StackTable struct {
	stack  []release // Stack releases, in schema order
	ledger []release // Ledger releases: carry the numscript-interpreter ceiling
}

// firstStack is the Stack-scale number of the first Stack release, 0 when the
// table is empty.
func (t *StackTable) firstStack() int {
	if len(t.stack) == 0 {
		return 0
	}
	scale, _ := stackScale(t.stack[0].version)
	return scale
}

// Releases returns the Stack versions of the table, in order, for reporting.
func (t *StackTable) Releases() []string {
	out := make([]string, len(t.stack))
	for i, r := range t.stack {
		out[i] = r.version
	}
	return out
}

// ParseSchema reads the Stack and Ledger release tables out of the docs repo's
// schema.ts. The file is TypeScript, not data, so this is a tolerant scanner
// rather than a parser: it finds the component by id, brace-matches its release
// list and reads `version:` and `requires:` from each entry. It fails loudly when
// the Stack block is missing or yields no release, because a table silently
// empty would widen every page to all versions.
func ParseSchema(src string) (*StackTable, error) {
	src = stripTSComments(src)
	stack, err := componentReleases(src, "stack")
	if err != nil {
		return nil, fmt.Errorf("schema.ts: %w", err)
	}
	if len(stack) == 0 {
		return nil, fmt.Errorf("schema.ts: component %q has no releases", "stack")
	}
	for _, r := range stack {
		if _, ok := stackScale(r.version); !ok {
			return nil, fmt.Errorf("schema.ts: stack release %q is not of the form vX.Y", r.version)
		}
	}
	// Ledger is optional: without it only the numscript-interpreter cascade is
	// lost, and those bounds are widened and listed rather than guessed.
	ledger, _ := componentReleases(src, "ledger")
	return &StackTable{stack: stack, ledger: ledger}, nil
}

// stripTSComments removes // and /* */ comments outside string literals.
func stripTSComments(src string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case quote != 0:
			b.WriteByte(c)
			if c == '\\' && i+1 < len(src) {
				i++
				b.WriteByte(src[i])
			} else if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'' || c == '`':
			quote = c
			b.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			b.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return b.String()
			}
			i += end + 3
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

var (
	versionFieldRe = regexp.MustCompile(`\bversion:\s*["']([^"']+)["']`)
	reqEntryRe     = regexp.MustCompile(`["']?([A-Za-z0-9_-]+)["']?\s*:\s*(?:["']([^"']*)["']|\{([^{}]*)\})`)
	minFieldRe     = regexp.MustCompile(`\bmin:\s*["']([^"']*)["']`)
	belowFieldRe   = regexp.MustCompile(`\bbelow:\s*["']([^"']*)["']`)
)

// componentReleases returns the releases of the component with the given id.
func componentReleases(src, id string) ([]release, error) {
	idRe := regexp.MustCompile(`\bid:\s*["']` + regexp.QuoteMeta(id) + `["']`)
	loc := idRe.FindStringIndex(src)
	if loc == nil {
		return nil, fmt.Errorf("component %q not found", id)
	}
	rest := src[loc[1]:]
	// The release list belongs to this component only if it opens before the next
	// component's id: a component without releases must not borrow the next one's.
	rel := strings.Index(rest, "releases:")
	next := regexp.MustCompile(`\bid:\s*["']`).FindStringIndex(rest)
	if rel < 0 || (next != nil && next[0] < rel) {
		return nil, fmt.Errorf("component %q has no releases list", id)
	}
	open := strings.Index(rest[rel:], "[")
	if open < 0 {
		return nil, fmt.Errorf("component %q: releases is not a list", id)
	}
	body, ok := matchBrackets(rest[rel+open:], '[', ']')
	if !ok {
		return nil, fmt.Errorf("component %q: unbalanced releases list", id)
	}

	var out []release
	for len(body) > 0 {
		o := strings.Index(body, "{")
		if o < 0 {
			break
		}
		entry, ok := matchBrackets(body[o:], '{', '}')
		if !ok {
			return nil, fmt.Errorf("component %q: unbalanced release entry", id)
		}
		body = body[o+len(entry)+2:]
		v := versionFieldRe.FindStringSubmatch(entry)
		if v == nil {
			return nil, fmt.Errorf("component %q: release entry without version: %.60q", id, entry)
		}
		r := release{version: v[1], requires: map[string]requirement{}}
		if ri := strings.Index(entry, "requires:"); ri >= 0 {
			if oi := strings.Index(entry[ri:], "{"); oi >= 0 {
				if reqBody, ok := matchBrackets(entry[ri+oi:], '{', '}'); ok {
					for _, m := range reqEntryRe.FindAllStringSubmatch(reqBody, -1) {
						req := requirement{min: m[2], point: m[3] == ""}
						if m[3] != "" {
							req = requirement{}
							if g := minFieldRe.FindStringSubmatch(m[3]); g != nil {
								req.min = g[1]
							}
							if g := belowFieldRe.FindStringSubmatch(m[3]); g != nil {
								req.below = g[1]
							}
						}
						r.requires[m[1]] = req
					}
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// matchBrackets takes s starting at an opening bracket and returns what lies
// between it and its match, skipping string literals.
func matchBrackets(s string, open, closeB byte) (string, bool) {
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case open:
			depth++
		case closeB:
			depth--
			if depth == 0 {
				return s[1:i], true
			}
		}
	}
	return "", false
}

// semver is a dotted numeric version. Anything else ("fctl-2.0", "latest")
// does not parse and is therefore not convertible.
type semver []int

var semverRe = regexp.MustCompile(`^v?(\d+(?:\.\d+)*)$`)

func parseSemver(s string) (semver, bool) {
	m := semverRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil, false
	}
	parts := strings.Split(m[1], ".")
	out := make(semver, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// cmp compares two versions, missing trailing parts counting as zero.
func (a semver) cmp(b semver) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// stackScale converts "v3.2", "3.2" or "3.2.1" to 302.
func stackScale(s string) (int, bool) {
	v, ok := parseSemver(s)
	if !ok || len(v) < 2 || v[0] > 99 || v[1] > 99 {
		return 0, false
	}
	return v[0]*100 + v[1], true
}

// span is a range of versions: [lo, hi), or [lo, hi] when hiIncl (a point
// version). A missing bound (hasLo / hasHi false) is unbounded on that side.
type span struct {
	lo, hi       semver
	hasLo, hasHi bool
	hiIncl       bool
}

// emptySpan overlaps nothing.
var emptySpan = span{lo: semver{0}, hi: semver{0}, hasLo: true, hasHi: true}

func (s span) empty() bool {
	if !s.hasLo || !s.hasHi {
		return false
	}
	c := s.lo.cmp(s.hi)
	return c > 0 || (c == 0 && !s.hiIncl)
}

// loBelowHi reports whether some version at or above lo lies below hi.
func loBelowHi(lo semver, hasLo bool, hi semver, hasHi, hiIncl bool) bool {
	if !hasLo || !hasHi {
		return true
	}
	c := lo.cmp(hi)
	return c < 0 || (c == 0 && hiIncl)
}

// overlaps reports whether the two ranges share at least one version. The upper
// bound is exclusive on both sides unless the range is a point version, so
// [2.3.0, 2.4.0) and [2.4.0, 2.5.0) do not overlap, and a point p overlaps
// [m, b) iff m <= p < b.
func (a span) overlaps(b span) bool {
	if a.empty() || b.empty() {
		return false
	}
	return loBelowHi(a.lo, a.hasLo, b.hi, b.hasHi, b.hiIncl) &&
		loBelowHi(b.lo, b.hasLo, a.hi, a.hasHi, a.hiIncl)
}

// span is the versions a release allows for a component. ok is false when the
// requirement is not a version ("fctl-2.0").
func (q requirement) span() (span, bool) {
	lo, ok := parseSemver(q.min)
	if !ok {
		return span{}, false
	}
	switch {
	case q.point:
		return span{lo: lo, hasLo: true, hi: lo, hasHi: true, hiIncl: true}, true
	case q.below == "":
		return span{lo: lo, hasLo: true}, true
	}
	hi, ok := parseSemver(q.below)
	if !ok {
		return span{}, false
	}
	return span{lo: lo, hasLo: true, hi: hi, hasHi: true}, true
}

// canonical maps the docs' component names onto the schema's ids.
func canonical(component string) string {
	if component == "flows" {
		return "orchestration"
	}
	return component
}

const interpreter = "numscript-interpreter"

// convertible reports whether some Stack release lists the component with a
// version the table can read. The interpreter is convertible through Ledger.
func (t *StackTable) convertible(component string) bool {
	if component == interpreter {
		if !t.convertible("ledger") {
			return false
		}
		for _, lr := range t.ledger {
			if req, ok := lr.requires[interpreter]; ok {
				if _, ok := req.span(); ok {
					return true
				}
			}
		}
		return false
	}
	for _, r := range t.stack {
		if req, ok := r.requires[component]; ok {
			if _, ok := req.span(); ok {
				return true
			}
		}
	}
	return false
}

// reaches reports whether any release allows a version at or above m. When none
// does, the bound is above everything the table knows (content for an
// unreleased version) and is widened rather than excluded.
func (t *StackTable) reaches(component string, m semver) bool {
	above := span{lo: m, hasLo: true}
	if component == interpreter {
		_, found := t.ledgerSpan(above)
		return found
	}
	for _, r := range t.stack {
		if req, ok := r.requires[component]; ok {
			if sp, ok := req.span(); !ok || sp.overlaps(above) {
				return true
			}
		}
	}
	return false
}

// ledgerSpan is the first step of the numscript-interpreter cascade. A Ledger
// release entry bundles an interpreter range and applies from its own version up
// to the next entry (the last one has no end). The entries whose interpreter range
// overlaps the content range c are collected, and their Ledger spans are merged
// into one hull [first start, last end): the Ledger versions an interpreter in c
// may be bundled in. Entries without an interpreter requirement bundle none and
// are skipped. The hull is a superset when the matching entries are not adjacent,
// which keeps the conversion conservative. found is false when no entry matches.
func (t *StackTable) ledgerSpan(c span) (hull span, found bool) {
	for i, lr := range t.ledger {
		req, ok := lr.requires[interpreter]
		if !ok {
			continue
		}
		sp, ok := req.span()
		lv, lok := parseSemver(lr.version)
		if !ok || !lok || !sp.overlaps(c) {
			continue
		}
		end, hasEnd := semver(nil), false
		if i+1 < len(t.ledger) {
			end, hasEnd = parseSemver(t.ledger[i+1].version)
		}
		if !found {
			hull, found = span{lo: lv, hasLo: true, hi: end, hasHi: hasEnd}, true
			continue
		}
		if lv.cmp(hull.lo) < 0 {
			hull.lo = lv
		}
		if hull.hasHi && (!hasEnd || end.cmp(hull.hi) > 0) {
			hull.hi, hull.hasHi = end, hasEnd
		}
	}
	return hull, found
}

// convertComponent converts the bounds of a component that is not the Stack
// itself, by interval intersection (see docs/kb-contract-changes.md section 3).
//
// Each Stack release allows a range of the component's versions. The content
// range C = [m, b) applies to a release iff the two overlap: the customer's exact
// patch version is unknown, so any overlap keeps the release. version_min is the
// first release that overlaps and version_below is the release after the last one
// that does, so the result is [first, last+1). Overlapping releases are searched
// only until the component leaves the Stack (the first release that no longer
// lists it): that release does not overlap and is the cap.
//
// numscript-interpreter is not in the Stack table, so it is composed in two
// steps with the same rule: C is intersected with each Ledger release's bundled
// interpreter range (ledgerSpan), the matching releases give a Ledger version
// span, and that span replaces C in the ordinary intersection over the Stack
// releases' "ledger" ranges (stack -> ledger -> numscript-interpreter).
//
// A bound that cannot be converted (component not in the table, unparsable
// version, or above every release) is widened on its own side only.
func (t *StackTable) convertComponent(c *Conversion) {
	comp := canonical(c.Component)
	var m, b semver
	var haveM, haveB bool
	if c.MinRaw != "" {
		if v, ok := parseSemver(c.MinRaw); ok {
			m, haveM = v, true
		} else {
			c.WidenedMin = true
		}
	}
	if c.BelowRaw != "" {
		if v, ok := parseSemver(c.BelowRaw); ok {
			b, haveB = v, true
		} else {
			c.WidenedBelow = true
		}
	}
	if !t.convertible(comp) {
		c.WidenedMin = c.MinRaw != ""
		c.WidenedBelow = c.BelowRaw != ""
		return
	}
	if haveM && !t.reaches(comp, m) {
		c.WidenedMin, haveM = true, false
	}

	content := span{lo: m, hasLo: haveM, hi: b, hasHi: haveB}
	target := comp
	if comp == interpreter {
		target = "ledger"
		if hull, found := t.ledgerSpan(content); found && !content.empty() {
			content = hull
		} else {
			content = emptySpan
		}
	}

	listed := make([]bool, len(t.stack))
	hit := make([]bool, len(t.stack))
	first := -1
	for i, r := range t.stack {
		req, ok := r.requires[target]
		listed[i] = ok
		if !ok {
			continue
		}
		// A requirement the table cannot read ("fctl-2.0") cannot exclude a release.
		sp, ok := req.span()
		hit[i] = !content.empty() && (!ok || sp.overlaps(content))
		if hit[i] && first < 0 {
			first = i
		}
	}
	if first < 0 {
		// Nothing overlaps. Widened bounds stay wide; otherwise no release ships
		// the range.
		if !c.Widened() {
			c.Empty = true
			c.Min, c.Below = 0, 0
		}
		return
	}

	end := first // the run of releases that list the component
	for end < len(t.stack) && listed[end] {
		end++
	}
	last := first
	for i := first; i < end; i++ {
		if hit[i] {
			last = i
		}
	}
	for i := first + 1; i < last; i++ {
		if !hit[i] {
			c.NonContiguous = true
		}
	}
	for i := end; i < len(t.stack); i++ {
		if hit[i] {
			c.NonContiguous = true // listed again after leaving: the cap wins
		}
	}

	if haveM {
		c.Min, _ = stackScale(t.stack[first].version)
	}
	if last+1 < len(t.stack) {
		c.Below, _ = stackScale(t.stack[last+1].version)
		c.Capped = last+1 == end
	}
	if c.WidenedBelow {
		c.Below, c.Capped = NoUpperBound, false
	}
}
