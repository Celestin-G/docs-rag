package chunker

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

// MDX cleaning — applied outside code fences only.
var (
	fctlCmdRe      = regexp.MustCompile(`\{"([^"]+)"\}`)
	docCardTitleRe = regexp.MustCompile(`title="([^"]+)"`)
	jsxOpenStartRe = regexp.MustCompile(`^<[A-Z][A-Za-z.]*`)
	jsxTagRe       = regexp.MustCompile(`</?[A-Z][A-Za-z.]*(?:\s[^>]*)?>`)
	divTagRe       = regexp.MustCompile(`</?div(?:\s[^>]*)?>`)
	multiBlankRe   = regexp.MustCompile(`\n{3,}`)

	// Frontmatter metadata extraction.
	badgeRe      = regexp.MustCompile(`(?m)^badge:\s*(\S+)`)
	versionMinRe = regexp.MustCompile(`min:\s*"v?(\d+)\.(\d+)`)
	pageTitleRe  = regexp.MustCompile(`(?m)^title:\s*(.+)$`)
	// A page may scope its version constraint to a component other than the
	// stack, e.g. `component: numscript-interpreter` with `version: {min: "0.0.15"}`.
	componentRe = regexp.MustCompile(`(?m)^component:\s*(\S+)`)
	// Pages declare which dimension values they apply to, e.g.
	// `visibility: { numscript: "new" }` — the page exists only when the New
	// Interpreter is selected. This is how the site hides experimental features,
	// and it is a different mechanism from the version bounds above.
	visibilityRe = regexp.MustCompile(`(?m)^visibility:\s*\{([^}]*)\}`)
	visEntryRe   = regexp.MustCompile(`([A-Za-z0-9_-]+)\s*:\s*(\[[^\]]*\]|"[^"]*"|'[^']*'|[^,\s]+)`)
)

// stripMDX removes JSX/MDX components while preserving useful text content.
// Code fences are left untouched. fctl commands and DocCard titles are extracted.
func stripMDX(content []byte) []byte {
	lines := strings.Split(string(content), "\n")
	out := make([]string, 0, len(lines))
	inCode := false
	skipToClose := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track code fences — never modify content inside them.
		if strings.HasPrefix(trimmed, "```") {
			inCode = !inCode
			out = append(out, line)
			continue
		}
		if inCode {
			out = append(out, line)
			continue
		}

		// Multi-line JSX opening tag: skip until the tag closes.
		if skipToClose {
			if strings.HasSuffix(trimmed, ">") || strings.HasSuffix(trimmed, "/>") {
				skipToClose = false
			}
			continue
		}

		// Drop import statements.
		if strings.HasPrefix(trimmed, "import ") && strings.Contains(trimmed, " from ") {
			continue
		}

		// <DocAPI.Fctl>{"command"}</DocAPI.Fctl> → extract the command verbatim.
		if strings.Contains(line, "<DocAPI.Fctl") {
			if m := fctlCmdRe.FindStringSubmatch(line); len(m) > 1 {
				out = append(out, m[1])
			}
			continue
		}

		// <DocCard title="X" ...> → emit the title as bold text.
		if strings.Contains(trimmed, "<DocCard") {
			if m := docCardTitleRe.FindStringSubmatch(trimmed); len(m) > 1 {
				out = append(out, "**"+m[1]+"**")
			}
			if !strings.HasSuffix(trimmed, ">") {
				skipToClose = true
			}
			continue
		}

		// Multi-line JSX opening tag with no closing > on this line.
		if jsxOpenStartRe.MatchString(trimmed) &&
			!strings.HasSuffix(trimmed, ">") &&
			!strings.HasSuffix(trimmed, "/>") {
			skipToClose = true
			continue
		}

		// Strip single-line JSX and div tags, keep surrounding text.
		cleaned := jsxTagRe.ReplaceAllString(line, "")
		cleaned = divTagRe.ReplaceAllString(cleaned, "")

		// Discard lines that were purely a JSX tag.
		if strings.TrimSpace(cleaned) == "" && trimmed != "" {
			continue
		}
		out = append(out, cleaned)
	}

	result := strings.Join(out, "\n")
	result = multiBlankRe.ReplaceAllString(result, "\n\n")
	return []byte(strings.TrimSpace(result))
}

// maxChunkBytes is ~500 tokens at ~4 chars/token.
const maxChunkBytes = 2000

// Chunk represents a parsed documentation section with its metadata.
type Chunk struct {
	SourceFile string
	Section    string
	Content    string
	Badge      string // "enterprise" or ""
	VersionNum int16  // lower bound: 0=unbounded, 30=v3.0, 31=v3.1, 32=v3.2, 33=v3.3
	// VersionBelow is the exclusive upper bound, 0 when the content still applies
	// to the latest version. Set from the docs framework's version-composition
	// ranges; markdown pages carry only a lower bound and leave this at 0.
	VersionBelow int16
	// VersionComponent names what VersionNum/VersionBelow are measured against:
	// "stack" by default, or a component id from the page's frontmatter such as
	// "numscript-interpreter". Bounds on other components are on that component's
	// own scale and must not be compared against a stack version.
	VersionComponent string
	Deployment       string // "cloud", "self-hosted", or "both"
	// Visibility maps a dimension id to the values the page applies to, e.g.
	// {"numscript": ["new"]}. A dimension absent from the map is unconstrained.
	// Values are always normalised to a slice, even when the frontmatter gives a
	// single string, so consumers never have to handle both shapes.
	Visibility map[string][]string
}

// pageMeta is what a page's frontmatter and path say about it.
type pageMeta struct {
	badge      string
	versionNum int16
	component  string // what versionNum is measured against; "stack" by default
	deployment string
	pageTitle  string
	visibility map[string][]string
}

// parseMeta extracts metadata from frontmatter and file path.
func parseMeta(relPath string, raw []byte) pageMeta {
	m := pageMeta{deployment: "both", component: "stack"}
	if strings.Contains(relPath, "deploy/cloud/") {
		m.deployment = "cloud"
	} else if strings.Contains(relPath, "deploy/self-hosted/") {
		m.deployment = "self-hosted"
	}

	// Extract frontmatter block only.
	if !bytes.HasPrefix(raw, []byte("---")) {
		return m
	}
	idx := bytes.Index(raw[3:], []byte("\n---"))
	if idx < 0 {
		return m
	}
	fm := string(raw[3 : 3+idx])

	if g := badgeRe.FindStringSubmatch(fm); len(g) > 1 {
		m.badge = strings.TrimSpace(g[1])
	}
	// Read the component before the version: the bound only means something
	// relative to it. A page scoped to numscript-interpreter states "0.0.15" on
	// that component's own scale, which is not comparable to a stack version.
	if g := componentRe.FindStringSubmatch(fm); len(g) > 1 {
		m.component = strings.Trim(strings.TrimSpace(g[1]), `"'`)
	}
	if g := versionMinRe.FindStringSubmatch(fm); len(g) > 2 {
		major, _ := strconv.Atoi(g[1])
		minor, _ := strconv.Atoi(g[2])
		m.versionNum = int16(major*10 + minor)
	}
	if g := pageTitleRe.FindStringSubmatch(fm); len(g) > 1 {
		m.pageTitle = strings.Trim(strings.TrimSpace(g[1]), `"'`)
	}
	if g := visibilityRe.FindStringSubmatch(fm); len(g) > 1 {
		m.visibility = parseVisibility(g[1])
	}
	return m
}

// parseVisibility reads the body of a `visibility: { ... }` frontmatter entry.
//
// Values come as a bare word, a quoted string or a list — "new", 'new' and
// ["new", "legacy"] all appear — and are normalised to a slice so the rest of
// the pipeline sees one shape. Written as a small scanner rather than a YAML
// dependency: the frontmatter is a fixed inline form, and a parse failure here
// should drop the constraint (page stays visible) rather than drop the page.
func parseVisibility(body string) map[string][]string {
	out := map[string][]string{}
	for _, g := range visEntryRe.FindAllStringSubmatch(body, -1) {
		key, raw := g[1], strings.TrimSpace(g[2])
		var values []string
		if strings.HasPrefix(raw, "[") {
			for _, item := range strings.Split(strings.Trim(raw, "[]"), ",") {
				if v := strings.Trim(strings.TrimSpace(item), `"'`); v != "" {
					values = append(values, v)
				}
			}
		} else if v := strings.Trim(raw, `"'`); v != "" {
			values = []string{v}
		}
		if len(values) > 0 {
			out[key] = values
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ParseFile parses a markdown or MDX file and returns chunks split by H2/H3 headings.
func ParseFile(relPath string, content []byte) []Chunk {
	meta := parseMeta(relPath, content)
	pageTitle := meta.pageTitle
	content = stripFrontmatter(content)
	content = stripMDX(content)
	sections := splitBySections(content)

	var chunks []Chunk
	for _, s := range sections {
		text := strings.TrimSpace(s.text)
		if text == "" {
			continue
		}
		for _, part := range splitBySize(text, maxChunkBytes) {
			// Build a full breadcrumb prefix: "Page Title > H2 > H3\n\nContent"
			var prefix string
			switch {
			case pageTitle != "" && s.heading != "":
				prefix = pageTitle + " > " + s.heading
			case pageTitle != "":
				prefix = pageTitle
			case s.heading != "":
				prefix = s.heading
			}
			body := part
			if prefix != "" {
				body = prefix + "\n\n" + part
			}
			chunks = append(chunks, Chunk{
				SourceFile:       relPath,
				Section:          s.heading,
				Content:          body,
				Badge:            meta.badge,
				VersionNum:       meta.versionNum,
				VersionComponent: meta.component,
				Deployment:       meta.deployment,
				Visibility:       meta.visibility,
			})
		}
	}
	return chunks
}

type section struct {
	heading string
	text    string
}

// stripFrontmatter removes YAML frontmatter delimited by --- lines.
func stripFrontmatter(content []byte) []byte {
	if !bytes.HasPrefix(content, []byte("---")) {
		return content
	}
	rest := content[3:]
	if len(rest) > 0 && rest[0] == '\r' {
		rest = rest[1:]
	}
	if len(rest) > 0 && rest[0] == '\n' {
		rest = rest[1:]
	}
	idx := bytes.Index(rest, []byte("\n---"))
	if idx < 0 {
		return content
	}
	after := rest[idx+4:]
	if len(after) > 0 && after[0] == '\r' {
		after = after[1:]
	}
	if len(after) > 0 && after[0] == '\n' {
		after = after[1:]
	}
	return after
}

// splitBySections splits the document by H2 (##) and H3 (###) headings.
// H3 chunks inherit their parent H2 as a breadcrumb prefix ("H2 > H3") so
// that embeddings capture the section hierarchy, not just the leaf label.
func splitBySections(content []byte) []section {
	var sections []section
	var current section
	var buf strings.Builder
	var parentH2 string

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if isH2(line) {
			if text := strings.TrimSpace(buf.String()); text != "" {
				current.text = text
				sections = append(sections, current)
			}
			parentH2 = extractHeadingText(line)
			current = section{heading: parentH2}
			buf.Reset()
		} else if isH3(line) {
			if text := strings.TrimSpace(buf.String()); text != "" {
				current.text = text
				sections = append(sections, current)
			}
			h3 := extractHeadingText(line)
			fullHeading := h3
			if parentH2 != "" {
				fullHeading = parentH2 + " > " + h3
			}
			current = section{heading: fullHeading}
			buf.Reset()
		} else {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}

	if text := strings.TrimSpace(buf.String()); text != "" {
		current.text = text
		sections = append(sections, current)
	}
	return sections
}

func isH2(line string) bool {
	return strings.HasPrefix(line, "## ") || line == "##"
}

func isH3(line string) bool {
	return strings.HasPrefix(line, "### ") || line == "###"
}

func extractHeadingText(line string) string {
	return strings.TrimSpace(strings.TrimLeft(line, "#"))
}

// splitBySize splits text into parts not exceeding maxBytes, splitting on paragraph boundaries.
func splitBySize(text string, maxBytes int) []string {
	if len(text) <= maxBytes {
		return []string{text}
	}

	paragraphs := strings.Split(text, "\n\n")
	var parts []string
	var buf strings.Builder

	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		extra := len(p)
		if buf.Len() > 0 {
			extra += 2
		}
		if buf.Len() > 0 && buf.Len()+extra > maxBytes {
			parts = append(parts, strings.TrimSpace(buf.String()))
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(p)
	}

	if s := strings.TrimSpace(buf.String()); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return []string{truncateAtWord(text, maxBytes)}
	}
	return parts
}

func truncateAtWord(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	truncated := s[:maxBytes]
	if idx := strings.LastIndex(truncated, " "); idx > 0 {
		truncated = truncated[:idx]
	}
	return truncated
}
