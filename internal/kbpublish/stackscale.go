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

// A Stack release lists, per component, the version it ships: a point version
// ("3.0.18") or a range ({min, below}). Component bounds in the docs (a page for
// payments 3.3.0, a block for ledger below 3.0.0) mean nothing to a customer who
// only knows their Stack version, so they are converted to the Stack scale with
// this table. The table is read from the docs repo's schema.ts at publish time
// rather than copied here, so it cannot drift.

type requirement struct {
	min, below string // below is empty for a point version
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
						req := requirement{min: m[2]}
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

// shipped is the version of a component a release ships: the lower bound of its
// range, or the point version.
func (r release) shipped(component string) (semver, bool) {
	req, ok := r.requires[component]
	if !ok {
		return nil, false
	}
	return parseSemver(req.min)
}

// canonical maps the docs' component names onto the schema's ids.
func canonical(component string) string {
	if component == "flows" {
		return "orchestration"
	}
	return component
}

// Lower converts a component's lower bound to the first Stack release that ships
// it. ok is false when the bound cannot be converted.
func (t *StackTable) Lower(component, version string) (int, bool) {
	component = canonical(component)
	if component == "stack" {
		return stackScale(version)
	}
	m, ok := parseSemver(version)
	if !ok {
		return 0, false
	}
	if component == "numscript-interpreter" {
		return t.viaLedger(m, true)
	}
	for _, r := range t.stack {
		if s, ok := r.shipped(component); ok && s.cmp(m) >= 0 {
			scale, _ := stackScale(r.version)
			return scale, true
		}
	}
	return 0, false
}

// Upper converts a component's exclusive upper bound to the first Stack release
// that ships a version at or above it (the feature is gone from there). A bound
// no release reaches means the feature is never removed: NoUpperBound.
func (t *StackTable) Upper(component, version string) (int, bool) {
	component = canonical(component)
	if component == "stack" {
		return stackScale(version)
	}
	b, ok := parseSemver(version)
	if !ok {
		return 0, false
	}
	if component == "numscript-interpreter" {
		return t.viaLedger(b, false)
	}
	if !t.knows(component) {
		return 0, false
	}
	for _, r := range t.stack {
		if s, ok := r.shipped(component); ok && s.cmp(b) >= 0 {
			scale, _ := stackScale(r.version)
			return scale, true
		}
	}
	return NoUpperBound, true
}

// knows reports whether any Stack release lists the component.
func (t *StackTable) knows(component string) bool {
	for _, r := range t.stack {
		if _, ok := r.requires[component]; ok {
			return true
		}
	}
	return false
}

// viaLedger handles numscript-interpreter, which the Stack does not list: each
// Ledger release bundles an interpreter and caps it at `below`. The first Ledger
// release whose cap is above the bound ships it, and that Ledger version is then
// converted through the Stack table (stack -> ledger -> numscript-interpreter).
func (t *StackTable) viaLedger(m semver, lower bool) (int, bool) {
	for _, lr := range t.ledger {
		req, ok := lr.requires["numscript-interpreter"]
		if !ok {
			continue
		}
		below, ok := parseSemver(req.below)
		if !ok || below.cmp(m) <= 0 {
			continue
		}
		lv, ok := parseSemver(lr.version)
		if !ok {
			return 0, false
		}
		for _, sr := range t.stack {
			if s, ok := sr.shipped("ledger"); ok && s.cmp(lv) >= 0 {
				scale, _ := stackScale(sr.version)
				return scale, true
			}
		}
		if lower {
			return 0, false
		}
		return NoUpperBound, true
	}
	if lower {
		return 0, false
	}
	return NoUpperBound, len(t.ledger) > 0
}
