package kbpublish

import (
	"fmt"
	"regexp"
	"strings"
)

// The docs gate paragraphs inline with <When component=… minimum=… below=…
// visibility={{…}}> and <DocCase value="ee|oss">. The site renders only the
// block that matches the reader's picker; a knowledge base has no picker, so
// stripping the tags would blend contradictory instructions into one passage.
// Each block is instead kept and prefixed with a note saying who it applies to.
// This runs on the raw MDX before the chunker sees it, on the publisher's path
// only, so the pgvector ingest is unchanged.

// Widening records a bound that could not be put on the Stack scale.
type Widening struct {
	File      string
	Inline    bool // false: the page's frontmatter; true: an inline block
	Component string
	Bound     string // the original bound, e.g. "min 3.3.0, below 3.4.0"
}

// Exclusion records a chunk or inline block left out because no Stack release
// ships its range. The fields mirror Widening.
type Exclusion = Widening

// InlineReport is what RewriteInline did to one file.
type InlineReport struct {
	WhenNotes    int
	DocCaseNotes int
	Widened      []Widening
	// Excluded lists the inline blocks dropped, with their text, because their
	// range is empty after conversion; Dropped counts every one (Excluded is
	// deduplicated).
	Excluded []Exclusion
	Dropped  int
	// Gaps lists blocks whose range overlaps non-adjacent releases (see
	// Conversion.NonContiguous).
	Gaps []Widening
	// Unhandled lists <When> or <DocCase> occurrences the rewriter did not
	// understand (an opening tag followed by text on the same line, say). They are
	// left to the chunker, which strips the tag, and reported so none is silent.
	Unhandled []string
}

var (
	attrRe       = func(name string) *regexp.Regexp { return regexp.MustCompile(`\b` + name + `=["']([^"']*)["']`) }
	componentAtt = attrRe("component")
	minimumAtt   = attrRe("minimum")
	belowAtt     = attrRe("below")
	visibilityAt = regexp.MustCompile(`visibility=\{\{([^}]*)\}\}`)
	visPairRe    = regexp.MustCompile(`([A-Za-z0-9_-]+)\s*:\s*(\[[^\]]*\]|"[^"]*"|'[^']*')`)
	docCaseRe    = regexp.MustCompile(`^<DocCase\s+value=["']([^"']*)["']\s*>$`)
)

// visibilityValues reads the body of a visibility={{ … }} attribute.
func visibilityValues(body string) map[string][]string {
	out := map[string][]string{}
	for _, m := range visPairRe.FindAllStringSubmatch(body, -1) {
		raw := strings.TrimSpace(m[2])
		raw = strings.Trim(raw, "[]")
		for item := range strings.SplitSeq(raw, ",") {
			if v := strings.Trim(strings.TrimSpace(item), `"'`); v != "" {
				out[m[1]] = append(out[m[1]], v)
			}
		}
	}
	return out
}

// audienceFrom maps visibility values to the audience phrases. A dimension
// listing several values constrains nothing; unknown dimensions (sdk, os, …) are
// presentation choices, not gating.
func audienceFrom(vis map[string][]string) Audience {
	one := func(dim string, allowed ...string) string {
		v := vis[dim]
		if len(v) != 1 {
			return ""
		}
		for _, a := range allowed {
			if v[0] == a {
				return a
			}
		}
		return ""
	}
	return Audience{
		License:    one("license", "ee", "oss"),
		Deployment: one("deployment", "cloud", "self-hosted"),
		Numscript:  one("numscript", "new", "legacy"),
	}
}

// widening describes the original bounds of a conversion.
func widening(file string, inline bool, c Conversion) Widening {
	var parts []string
	if c.MinRaw != "" {
		parts = append(parts, "min "+c.MinRaw)
	}
	if c.BelowRaw != "" {
		parts = append(parts, "below "+c.BelowRaw)
	}
	return Widening{File: file, Inline: inline, Component: c.Component, Bound: strings.Join(parts, ", ")}
}

// RewriteInline replaces each <When> opening tag with an "(Applies …)" note
// line and each <DocCase value="ee|oss"> with "(Enterprise Edition)" or "(Open
// source edition)". Frontmatter and code fences are left alone.
func RewriteInline(rel string, raw []byte, t *StackTable) ([]byte, InlineReport) {
	var rep InlineReport
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))

	i := 0
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		out = append(out, lines[0])
		for i = 1; i < len(lines); i++ {
			out = append(out, lines[i])
			if strings.TrimSpace(lines[i]) == "---" {
				i++
				break
			}
		}
	}

	inCode := false
	for ; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			out = append(out, line)
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}

		if m := docCaseRe.FindStringSubmatch(trimmed); m != nil {
			note := ""
			switch m[1] {
			case "ee":
				note = "(Enterprise Edition)"
			case "oss":
				note = "(Open source edition)"
			default:
				out = append(out, line)
				continue
			}
			if blockHasText(lines, i, "</DocCase>") {
				out = append(out, note, "")
				rep.DocCaseNotes++
			}
			continue
		}

		if !isWhenOpen(trimmed) {
			if strings.Contains(trimmed, "<When") || strings.Contains(trimmed, "<DocCase") {
				rep.Unhandled = append(rep.Unhandled, fmt.Sprintf("%s:%d: %s", rel, i+1, truncate(trimmed, 80)))
			}
			out = append(out, line)
			continue
		}

		// An opening tag may span lines: gather until one ends with ">".
		tag, start := trimmed, i
		for !strings.HasSuffix(strings.TrimSpace(tag), ">") && i+1 < len(lines) {
			i++
			tag += " " + strings.TrimSpace(lines[i])
		}
		attrs := tag
		hasFallback := false
		if k := strings.Index(attrs, "fallback="); k >= 0 {
			attrs, hasFallback = attrs[:k], true
		}
		// Anything after the first ">" of a fallback-less tag is block content on
		// the same line: not something to rewrite line-wise.
		if !hasFallback && strings.Index(tag, ">") != len(tag)-1 && !strings.HasSuffix(tag, "/>") {
			rep.Unhandled = append(rep.Unhandled, fmt.Sprintf("%s:%d: %s", rel, start+1, truncate(trimmed, 80)))
			out = append(out, lines[start:i+1]...)
			continue
		}

		get := func(re *regexp.Regexp) string {
			if m := re.FindStringSubmatch(attrs); m != nil {
				return m[1]
			}
			return ""
		}
		comp := get(componentAtt)
		if comp == "" {
			comp = "stack"
		}
		var aud Audience
		if m := visibilityAt.FindStringSubmatch(attrs); m != nil {
			aud = audienceFrom(visibilityValues(m[1]))
		}
		conv := t.Convert(comp, get(minimumAtt), get(belowAtt))
		if conv.Empty {
			// No Stack release ships this range: the block is dropped, text and
			// all, rather than shown to everyone.
			if end := blockEnd(lines, i, "</When>"); end >= 0 {
				if blockHasText(lines, i, "</When>") {
					rep.Excluded = append(rep.Excluded, widening(rel, true, conv))
					rep.Dropped++
				}
				i = end
				continue
			}
			rep.Unhandled = append(rep.Unhandled, fmt.Sprintf("%s:%d: empty range but no closing tag: %s", rel, start+1, truncate(trimmed, 60)))
			out = append(out, lines[start:i+1]...)
			continue
		}
		note := InlineNote(conv, aud)
		// A block that produces no text (only components the chunker strips) gets
		// no note: it would dangle in front of the next paragraph.
		if (note != "" || conv.Widened()) && !blockHasText(lines, i, "</When>") {
			continue
		}
		if conv.Widened() {
			rep.Widened = append(rep.Widened, widening(rel, true, conv))
		}
		if conv.NonContiguous {
			rep.Gaps = append(rep.Gaps, widening(rel, true, conv))
		}
		if note != "" {
			out = append(out, note, "")
			rep.WhenNotes++
		}
		// A block that constrains nothing loses its tag line, as the chunker does.
	}
	return []byte(strings.Join(out, "\n")), rep
}

var (
	strippedTagRe = regexp.MustCompile(`</?[A-Z][A-Za-z.]*(?:\s[^>]*)?>|</?div(?:\s[^>]*)?>`)
	jsxOpenRe     = regexp.MustCompile(`^<[A-Z][A-Za-z.]*`)
)

// blockEnd returns the line of the closing tag of the block opened on
// lines[open], -1 when there is none. Nested <When> blocks are matched.
func blockEnd(lines []string, open int, closeTag string) int {
	depth := 1
	for j := open + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if closeTag == "</When>" && isWhenOpen(t) {
			depth++
		}
		if strings.HasPrefix(t, closeTag) {
			if depth--; depth == 0 {
				return j
			}
		}
	}
	return -1
}

// blockHasText reports whether the block opened on lines[open], up to its
// closing tag, keeps any text once the chunker has stripped the JSX components
// (it mirrors chunker.stripMDX). A block with no closing tag is assumed to have
// text, so a note is never dropped on a guess.
func blockHasText(lines []string, open int, closeTag string) bool {
	end := blockEnd(lines, open, closeTag)
	if end < 0 {
		return true
	}
	skipToClose := false
	for _, line := range lines[open+1 : end] {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "```"):
			return true
		case skipToClose:
			if strings.HasSuffix(t, ">") {
				skipToClose = false
			}
			continue
		case strings.HasPrefix(t, "import ") && strings.Contains(t, " from "):
			continue
		case strings.Contains(t, "<DocAPI.Fctl"), strings.Contains(t, "<DocCard"):
			return true // the chunker keeps the command or the card title
		case jsxOpenRe.MatchString(t) && !strings.HasSuffix(t, ">"):
			skipToClose = true
			continue
		}
		if strings.TrimSpace(strippedTagRe.ReplaceAllString(line, "")) != "" {
			return true
		}
	}
	return false
}

// isWhenOpen reports whether a trimmed line starts a <When> opening tag (and not
// another component whose name begins with "When").
func isWhenOpen(trimmed string) bool {
	rest, ok := strings.CutPrefix(trimmed, "<When")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '>')
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
