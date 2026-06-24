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
	SourceFile  string
	Section     string
	Content     string
	Badge       string // "enterprise" or ""
	VersionNum  int16  // 0=no constraint, 30=v3.0, 31=v3.1, 32=v3.2, 33=v3.3
	Deployment  string // "cloud", "self-hosted", or "both"
}

// parseMeta extracts badge, version, and deployment from frontmatter + file path.
func parseMeta(relPath string, raw []byte) (badge string, versionNum int16, deployment string) {
	deployment = "both"
	if strings.Contains(relPath, "deploy/cloud/") {
		deployment = "cloud"
	} else if strings.Contains(relPath, "deploy/self-hosted/") {
		deployment = "self-hosted"
	}

	// Extract frontmatter block only.
	if !bytes.HasPrefix(raw, []byte("---")) {
		return
	}
	idx := bytes.Index(raw[3:], []byte("\n---"))
	if idx < 0 {
		return
	}
	fm := string(raw[3 : 3+idx])

	if m := badgeRe.FindStringSubmatch(fm); len(m) > 1 {
		badge = strings.TrimSpace(m[1])
	}
	if m := versionMinRe.FindStringSubmatch(fm); len(m) > 2 {
		major, _ := strconv.Atoi(m[1])
		minor, _ := strconv.Atoi(m[2])
		versionNum = int16(major*10 + minor)
	}
	return
}

// ParseFile parses a markdown or MDX file and returns chunks split by H2/H3 headings.
func ParseFile(relPath string, content []byte) []Chunk {
	badge, versionNum, deployment := parseMeta(relPath, content)
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
			body := part
			if s.heading != "" {
				body = s.heading + "\n\n" + part
			}
			chunks = append(chunks, Chunk{
				SourceFile: relPath,
				Section:    s.heading,
				Content:    body,
				Badge:      badge,
				VersionNum: versionNum,
				Deployment: deployment,
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