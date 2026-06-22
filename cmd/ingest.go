package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/chunker"
	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

var ingestCmd = &cobra.Command{
	Use:   "ingest",
	Short: "Parse documentation and store embeddings in pgvector",
	RunE:  runIngest,
}

func init() {
	ingestCmd.Flags().Bool("clean", false, "Truncate existing data before ingesting")
	rootCmd.AddCommand(ingestCmd)
}

func runIngest(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()

	docsPath := mustEnv("DOCS_PATH")
	dbURL := mustEnv("DATABASE_URL")

	clean, _ := cmd.Flags().GetBool("clean")

	st, err := store.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	if clean {
		fmt.Fprintln(os.Stderr, "Truncating existing data...")
		if err := st.Truncate(ctx); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
	}

	emb := embedder.New()

	var files []string
	err = filepath.WalkDir(docsPath, func(path string, d os.DirEntry, err error) error {
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
		if ext != ".md" && ext != ".mdx" {
			return nil
		}
		rel, _ := filepath.Rel(docsPath, path)
		if isDocFile(rel) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walk %s: %w", docsPath, err)
	}

	fmt.Fprintf(os.Stderr, "Found %d files in %s\n\n", len(files), docsPath)

	totalChunks := 0
	for i, f := range files {
		relPath, _ := filepath.Rel(docsPath, f)

		content, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", relPath, err)
			continue
		}

		chunks := chunker.ParseFile(relPath, content)
		if len(chunks) == 0 {
			continue
		}

		texts := make([]string, len(chunks))
		for j, c := range chunks {
			texts[j] = c.Content
		}

		embeddings, err := emb.EmbedBatch(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed %s: %w", relPath, err)
		}

		for j, c := range chunks {
			if err := st.Insert(ctx, store.DocChunk{
				SourceFile: c.SourceFile,
				Section:    c.Section,
				Content:    c.Content,
				Embedding:  embeddings[j],
			}); err != nil {
				return fmt.Errorf("insert chunk from %s: %w", relPath, err)
			}
		}

		totalChunks += len(chunks)
		fmt.Printf("[%d/%d] %-60s %d chunks\n", i+1, len(files), relPath, len(chunks))
	}

	fmt.Fprintf(os.Stderr, "\nDone — ingested %d chunks from %d files.\n", totalChunks, len(files))
	return nil
}

// isDocFile returns true only for files that are actual Formance user documentation.
// This excludes project meta-files (CLAUDE.md, README.md at root) and internal
// tooling packages (docs-framework, vcc) that pollute search results.
func isDocFile(rel string) bool {
	// Keep everything under apps/docs/content/ (pages + fragments)
	if strings.HasPrefix(rel, "apps/docs/content/") {
		return true
	}
	// Keep the numscript engine README which has useful technical content
	if strings.HasPrefix(rel, "packages/numscript-engine/") {
		return true
	}
	return false
}
