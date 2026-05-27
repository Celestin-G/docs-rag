package chunker

import (
	"bufio"
	"bytes"
	"strings"
)

// maxChunkBytes is ~500 tokens at ~4 chars/token.
const maxChunkBytes = 2000

// Chunk represents a parsed documentation section with its metadata.
type Chunk struct {
	SourceFile string
	Section    string
	Content    string
}

// ParseFile parses a markdown or MDX file and returns chunks split by H2/H3 headings.
func ParseFile(relPath string, content []byte) []Chunk {
	content = stripFrontmatter(content)
	sections := splitBySections(content)

	var chunks []Chunk
	for _, s := range sections {
		text := strings.TrimSpace(s.text)
		if text == "" {
			continue
		}
		for _, part := range splitBySize(text, maxChunkBytes) {
			chunks = append(chunks, Chunk{
				SourceFile: relPath,
				Section:    s.heading,
				Content:    part,
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
func splitBySections(content []byte) []section {
	var sections []section
	var current section
	var buf strings.Builder

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		if isHeading(line) {
			if text := strings.TrimSpace(buf.String()); text != "" {
				current.text = text
				sections = append(sections, current)
			}
			current = section{heading: extractHeadingText(line)}
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

func isHeading(line string) bool {
	return strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") ||
		line == "##" || line == "###"
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