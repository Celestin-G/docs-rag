package kbpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// githubSlug ports github-slugger's slug(): lowercase, drop everything but
// letters, marks, numbers, connector punctuation, spaces and hyphens, then turn
// each space into a hyphen. It is what the docs site's rehype-slug writes into
// heading ids, so the "#fragment" of a source_url lands on the right heading.
func githubSlug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ' ':
			b.WriteByte('-')
		case r == '-' || unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.IsMark(r) || unicode.Is(unicode.Pc, r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

// slugger deduplicates like github-slugger's instance: the second "Setup" on a
// page becomes "setup-1".
type slugger struct{ occurrences map[string]int }

func newSlugger() *slugger { return &slugger{occurrences: map[string]int{}} }

func (g *slugger) slug(value string) string {
	original := githubSlug(value)
	result := original
	for {
		if _, seen := g.occurrences[result]; !seen {
			break
		}
		g.occurrences[original]++
		result = original + "-" + strconv.Itoa(g.occurrences[original])
	}
	g.occurrences[result] = 0
	return result
}

// heading is one markdown heading of a page.
type heading struct {
	depth int
	raw   string // as the chunker sees it: text after the hashes
	id    string // the anchor the site gives it
}

var (
	fenceRe       = regexp.MustCompile("^\\s*(`{3,}|~{3,})(.*)$")
	headingLineRe = regexp.MustCompile(`^[ \t]*(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	trailHashRe   = regexp.MustCompile(`(^|[ \t]+)#+$`)
	linkRe        = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTagRe     = regexp.MustCompile(`<[^>]+>`)
	codeSpanRe    = regexp.MustCompile("`([^`]*)`")
	emphasisRe    = regexp.MustCompile(`(^|\W)[*_](.+?)[*_](\W|$)`)
	frontmatterRe = regexp.MustCompile(`^---\r?\n[\s\S]*?\r?\n---`)
)

// pageHeadings lists the headings of a raw MDX page with the ids the site gives
// them: one slugger per page over every heading level, fenced code skipped. It
// follows extractHeadings in packages/docs-framework (mdx-headings.mjs).
func pageHeadings(raw string) []heading {
	g := newSlugger()
	var out []heading
	var fence string
	for line := range strings.SplitSeq(frontmatterRe.ReplaceAllString(raw, ""), "\n") {
		line = strings.TrimSuffix(line, "\r")
		marker := fenceRe.FindStringSubmatch(line)
		if fence == "" {
			if marker != nil && !(marker[1][0] == '`' && strings.Contains(marker[2], "`")) {
				fence = marker[1]
				continue
			}
		} else {
			if marker != nil && marker[1][0] == fence[0] && len(marker[1]) >= len(fence) && strings.TrimSpace(marker[2]) == "" {
				fence = ""
			}
			continue
		}
		m := headingLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rawText := strings.TrimSpace(trailHashRe.ReplaceAllString(m[2], ""))
		text := rawText
		text = linkRe.ReplaceAllString(text, "$1")
		text = htmlTagRe.ReplaceAllString(text, "")
		text = codeSpanRe.ReplaceAllString(text, "$1")
		text = replaceStrong(text)
		text = emphasisRe.ReplaceAllString(text, "$1$2$3")
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		out = append(out, heading{depth: len(m[1]), raw: chunkerHeadingText(line), id: g.slug(text)})
	}
	return out
}

// replaceStrong turns **x** and __x__ into x, requiring the same delimiter on
// both sides as the JavaScript backreference does.
func replaceStrong(s string) string {
	for _, d := range []string{"**", "__"} {
		re := regexp.MustCompile(regexp.QuoteMeta(d) + `(.+?)` + regexp.QuoteMeta(d))
		s = re.ReplaceAllString(s, "$1")
	}
	return s
}

// chunkerHeadingText is how the chunker names a heading line: the text after
// the leading hashes, trimmed (chunker.extractHeadingText).
func chunkerHeadingText(line string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
}

// Key helpers --------------------------------------------------------------

var nonKeyChar = regexp.MustCompile(`[^a-z0-9._]+`)

// keySlug turns free text into a key segment: lowercase [a-z0-9._-], at most 60
// characters, and when the text had to be cut the first 8 hex digits of its hash
// are appended so two long titles with the same start stay distinct.
func keySlug(s string) string {
	slug := strings.Trim(nonKeyChar.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if slug == "" {
		slug = "section"
	}
	if len(slug) <= 60 {
		return slug
	}
	sum := sha256.Sum256([]byte(s))
	return strings.Trim(slug[:60], "-.") + "-" + hex.EncodeToString(sum[:])[:8]
}

// keySegments normalises a URL or file path into key segments.
func keySegments(p string) []string {
	var out []string
	for seg := range strings.SplitSeq(strings.Trim(p, "/"), "/") {
		if seg == "" {
			continue
		}
		out = append(out, keySlug(seg))
	}
	return out
}
