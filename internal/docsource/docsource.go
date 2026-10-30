// Package docsource knows which files of the formancehq/docs checkout are
// documentation and how to turn one of them into chunks. It is shared by the
// pgvector ingest and the knowledge-base publisher, so both read exactly the
// same corpus.
package docsource

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/chunker"
)

// Kinds of documentation file, as reported by Kind.
const (
	KindPage            = "pages"
	KindFragment        = "fragments"
	KindGenerated       = "generated"
	KindNumscriptEngine = "numscript-engine"
)

// Walk returns the absolute path of every documentation file under root, in
// lexical order. Hidden directories, node_modules and .docusaurus are skipped.
func Walk(root string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == ".docusaurus" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".md" && ext != ".mdx" && ext != ".json" {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if IsDocFile(filepath.ToSlash(rel)) {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// Parse turns one documentation file into chunks. The generated data files are
// JSON envelopes rather than markdown, so they take a different parser.
func Parse(rel string, content []byte) []chunker.Chunk {
	if strings.EqualFold(filepath.Ext(rel), ".json") {
		return chunker.ParseGeneratedJSON(rel, content)
	}
	return chunker.ParseFile(rel, content)
}

// SafeParse is Parse with a panic turned into an error, used only by dry runs.
// A real ingest lets a panic crash: a partially filled index is worse than an
// obvious failure.
func SafeParse(rel string, content []byte) (chunks []chunker.Chunk, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return Parse(rel, content), nil
}

// Kind classifies a documentation file by its repo-relative slash path. It
// returns "" for a file IsDocFile rejects.
func Kind(rel string) string {
	switch {
	case !IsDocFile(rel):
		return ""
	case strings.HasSuffix(rel, ".json"):
		return KindGenerated
	case strings.HasPrefix(rel, "apps/docs/content/pages/"):
		return KindPage
	case strings.HasPrefix(rel, "apps/docs/content/fragments/"):
		return KindFragment
	}
	return KindNumscriptEngine
}

// IsDocFile returns true only for files that are actual Formance user documentation.
// This excludes project meta-files (CLAUDE.md, README.md at root) and internal
// tooling packages (docs-framework, vcc) that pollute search results.
func IsDocFile(rel string) bool {
	// The generated data files are the only JSON worth reading: they hold the API
	// operations, request/response schemas and fctl commands that the reference
	// pages render at build time. Every other JSON in the repo is tooling config.
	if strings.HasSuffix(rel, ".json") {
		return strings.HasPrefix(rel, "apps/docs/content/data/generated/")
	}
	// Named directories rather than all of apps/docs/content/. That directory
	// also holds authoring templates and internal worklists, which are not
	// published — and an allowlist means the next thing filed there stays out by
	// default instead of quietly joining the index.
	//
	// Found through the version picker: the only page claiming stack 3.3 was
	// connector-page.template.mdx, a blank template carrying 3.3 as an example
	// value in its frontmatter. It invented a version that does not exist, and
	// its boilerplate competed with real pages on connector questions.
	for _, dir := range []string{
		"apps/docs/content/pages/",     // the published documentation
		"apps/docs/content/fragments/", // partials the pages include
	} {
		if strings.HasPrefix(rel, dir) {
			return true
		}
	}
	// Keep the numscript engine README which has useful technical content
	if strings.HasPrefix(rel, "packages/numscript-engine/") {
		return true
	}
	return false
}
