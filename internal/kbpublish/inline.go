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
	Reason    string // "not convertible" or "empty range after conversion"
}

// InlineReport is what RewriteInline did to one file.
type InlineReport struct {
	WhenNotes    int
	DocCaseNotes int
	Widened      []Widening
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

func widening(file string, inline bool, c Conversion) Widening {
	var parts []string
	if c.MinRaw != "" {
		parts = append(parts, "min "+c.MinRaw)
	}
	if c.BelowRaw != "" {
		parts = append(parts, "below "+c.BelowRaw)
	}
	reason := "not convertible"
	if c.Empty {
		reason = "empty range after conversion"
	}
	return Widening{File: file, Inline: inline, Component: c.Component, Bound: strings.Join(parts, ", "), Reason: reason}
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
			switch m[1] {
			case "ee":
				out = append(out, "(Enterprise Edition)", "")
				rep.DocCaseNotes++
				continue
			case "oss":
				out = append(out, "(Open source edition)", "")
				rep.DocCaseNotes++
				continue
			}
			out = append(out, line)
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
		if conv.Widened() {
			rep.Widened = append(rep.Widened, widening(rel, true, conv))
		}
		if note := InlineNote(conv, aud); note != "" {
			out = append(out, note, "")
			rep.WhenNotes++
		}
		// A block that constrains nothing loses its tag line, as the chunker does.
	}
	return []byte(strings.Join(out, "\n")), rep
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
