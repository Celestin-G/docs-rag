package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	ingestCmd.Flags().Bool("dry-run", false, "Parse every file and report what would be ingested, without embedding or writing")
	rootCmd.AddCommand(ingestCmd)
}

func runIngest(cmd *cobra.Command, _ []string) error {
	ctx := context.Background()

	docsPath := mustEnv("DOCS_PATH")

	clean, _ := cmd.Flags().GetBool("clean")
	dry, _ := cmd.Flags().GetBool("dry-run")

	// A dry run touches nothing outside this process: no database, no embedder,
	// no DATABASE_URL required. That is the point — a real run truncates before
	// it refills, so a parser that dies halfway leaves the index empty until
	// someone notices. This exercises the parsing on the real corpus first.
	var st *store.Store
	var emb *embedder.Embedder
	if !dry {
		st2, err := store.New(ctx, mustEnv("DATABASE_URL"))
		if err != nil {
			return fmt.Errorf("connect to database: %w", err)
		}
		defer st2.Close()
		st = st2

		if clean {
			fmt.Fprintln(os.Stderr, "Truncating existing data...")
			if err := st.Truncate(ctx); err != nil {
				return fmt.Errorf("truncate: %w", err)
			}
		}
		emb = embedder.New()
	}

	var files []string
	err := filepath.WalkDir(docsPath, func(path string, d os.DirEntry, err error) error {
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

	report := newIngestReport()
	totalChunks := 0
	for i, f := range files {
		relPath, _ := filepath.Rel(docsPath, f)

		content, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", relPath, err)
			continue
		}

		var chunks []chunker.Chunk
		if dry {
			// Keep going after a bad file: the point of a dry run is the full list
			// of what would break, not the first thing that does.
			chunks, err = safeParse(relPath, content)
			if err != nil {
				report.failed = append(report.failed, fmt.Sprintf("%s: %v", relPath, err))
				continue
			}
			report.add(relPath, chunks)
			continue
		}
		chunks = parseDoc(relPath, content)
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
			}, c.Badge, c.VersionNum, c.VersionBelow, c.VersionComponent, c.Deployment, c.Visibility); err != nil {
				return fmt.Errorf("insert chunk from %s: %w", relPath, err)
			}
		}

		totalChunks += len(chunks)
		fmt.Printf("[%d/%d] %-60s %d chunks\n", i+1, len(files), relPath, len(chunks))
	}

	if dry {
		return report.print(os.Stderr, len(files))
	}
	fmt.Fprintf(os.Stderr, "\nDone — ingested %d chunks from %d files.\n", totalChunks, len(files))
	return nil
}

// parseDoc turns one documentation file into chunks. The generated data files
// are JSON envelopes rather than markdown, so they take a different parser.
func parseDoc(rel string, content []byte) []chunker.Chunk {
	if strings.EqualFold(filepath.Ext(rel), ".json") {
		return chunker.ParseGeneratedJSON(rel, content)
	}
	return chunker.ParseFile(rel, content)
}

// safeParse is parseDoc with a panic turned into an error, used only by the dry
// run. A real ingest lets a panic crash: a partially filled index is worse than
// an obvious failure.
func safeParse(rel string, content []byte) (chunks []chunker.Chunk, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return parseDoc(rel, content), nil
}

// ingestReport is what a dry run observed. The counts exist to be compared
// against the previous run: a dimension that drops to zero means a frontmatter
// key changed name upstream, which no test can catch because the docs live in
// another repository.
type ingestReport struct {
	files, chunks int
	gated         int            // chunks carrying a visibility constraint
	byExt         map[string]int // ".mdx" → chunks
	badges        map[string]int // badge → chunks
	components    map[string]int // version component → chunks
	dimensions    map[string]int // visibility dimension → chunks
	largest       []fileCount    // biggest producers, kept sorted
	empty         []string       // files that parsed to nothing
	failed        []string       // files that could not be parsed at all
}

type fileCount struct {
	path string
	n    int
}

func newIngestReport() *ingestReport {
	return &ingestReport{
		byExt:      map[string]int{},
		badges:     map[string]int{},
		components: map[string]int{},
		dimensions: map[string]int{},
	}
}

func (r *ingestReport) add(rel string, chunks []chunker.Chunk) {
	r.files++
	if len(chunks) == 0 {
		r.empty = append(r.empty, rel)
		return
	}
	r.chunks += len(chunks)
	r.byExt[strings.ToLower(filepath.Ext(rel))] += len(chunks)
	for _, c := range chunks {
		if c.Badge != "" {
			r.badges[c.Badge]++
		}
		if c.VersionComponent != "" {
			r.components[c.VersionComponent]++
		}
		if len(c.Visibility) > 0 {
			r.gated++
			for dim := range c.Visibility {
				r.dimensions[dim]++
			}
		}
	}
	r.largest = append(r.largest, fileCount{rel, len(chunks)})
	sort.Slice(r.largest, func(i, j int) bool { return r.largest[i].n > r.largest[j].n })
	if len(r.largest) > 5 {
		r.largest = r.largest[:5]
	}
}

// print writes the summary and returns an error when the run found something
// that would break a real ingest, so the exit code alone is a usable signal.
func (r *ingestReport) print(w io.Writer, found int) error {
	fmt.Fprintf(w, "\nDry run — parsed %d of %d files, %d chunks, nothing written.\n", r.files, found, r.chunks)

	section := func(title string, counts map[string]int) {
		if len(counts) == 0 {
			return
		}
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return counts[keys[i]] > counts[keys[j]] })
		fmt.Fprintf(w, "\n%s\n", title)
		for _, k := range keys {
			fmt.Fprintf(w, "  %-24s %6d\n", k, counts[k])
		}
	}
	section("Chunks by file type", r.byExt)
	section("Chunks by version component", r.components)
	section("Chunks by badge", r.badges)
	section(fmt.Sprintf("Gated chunks by dimension (%d gated in total)", r.gated), r.dimensions)

	if len(r.largest) > 0 {
		fmt.Fprintf(w, "\nLargest files\n")
		for _, f := range r.largest {
			fmt.Fprintf(w, "  %-70s %6d\n", f.path, f.n)
		}
	}

	// Listed in full rather than sampled: a file the ingest silently drops is
	// documentation the assistant cannot answer from, and the only way anyone
	// notices is by reading this list.
	if n := len(r.empty); n > 0 {
		fmt.Fprintf(w, "\n%d files produced no chunk:\n", n)
		for _, p := range r.empty {
			fmt.Fprintf(w, "  %s\n", p)
		}
	}

	if len(r.failed) > 0 {
		fmt.Fprintf(w, "\n%d files failed to parse:\n", len(r.failed))
		for _, p := range r.failed {
			fmt.Fprintf(w, "  %s\n", p)
		}
		return fmt.Errorf("%d files would break the ingest", len(r.failed))
	}
	if r.chunks == 0 {
		return fmt.Errorf("no chunks produced — a real run would empty the index")
	}
	fmt.Fprintln(w, "\nNo parse failure. A real run would refill the index.")
	return nil
}

// isDocFile returns true only for files that are actual Formance user documentation.
// This excludes project meta-files (CLAUDE.md, README.md at root) and internal
// tooling packages (docs-framework, vcc) that pollute search results.
func isDocFile(rel string) bool {
	// The generated data files are the only JSON worth reading: they hold the API
	// operations, request/response schemas and fctl commands that the reference
	// pages render at build time. Every other JSON in the repo is tooling config.
	if strings.HasSuffix(rel, ".json") {
		return strings.HasPrefix(rel, "apps/docs/content/data/generated/")
	}
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
