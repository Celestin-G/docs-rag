package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/embedder"
	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/store"
	"github.com/spf13/cobra"
)

var askCmd = &cobra.Command{
	Use:   "ask <question>",
	Short: "Ask a question about Formance documentation",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runAsk,
}

func init() {
	askCmd.Flags().Int("top-k", 5, "Number of chunks to retrieve")
	rootCmd.AddCommand(askCmd)
}

func runAsk(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	question := strings.Join(args, " ")
	openaiKey := mustEnv("OPENAI_API_KEY")
	anthropicKey := mustEnv("ANTHROPIC_API_KEY")
	dbURL := mustEnv("DATABASE_URL")
	topK, _ := cmd.Flags().GetInt("top-k")

	st, err := store.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()

	emb := embedder.New(openaiKey)

	fmt.Fprintln(os.Stderr, "Generating embedding for question...")
	vec, err := emb.Embed(ctx, question)
	if err != nil {
		return fmt.Errorf("embed question: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Searching top %d chunks...\n", topK)
	chunks, err := st.Search(ctx, vec, topK)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(chunks) == 0 {
		fmt.Println("No relevant documentation found.")
		return nil
	}

	fmt.Fprintf(os.Stderr, "Found %d chunks. Generating answer...\n\n", len(chunks))

	ctxChunks := make([]llm.ContextChunk, len(chunks))
	for i, c := range chunks {
		ctxChunks[i] = llm.ContextChunk{
			SourceFile: c.SourceFile,
			Section:    c.Section,
			Content:    c.Content,
		}
	}

	claude := llm.New(anthropicKey)
	return claude.Ask(ctx, question, ctxChunks, os.Stdout)
}
