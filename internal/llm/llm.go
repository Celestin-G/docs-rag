package llm

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Client wraps the Anthropic messages API.
type Client struct {
	client anthropic.Client
}

// New creates a Client using the given Anthropic API key.
func New(apiKey string) *Client {
	return &Client{
		client: anthropic.NewClient(option.WithAPIKey(apiKey)),
	}
}

// ContextChunk is a retrieved doc chunk used as RAG context.
type ContextChunk struct {
	SourceFile string
	Section    string
	Content    string
}

// Ask streams a response to the given question using the provided chunks as context.
func (c *Client) Ask(ctx context.Context, question string, chunks []ContextChunk, out io.Writer) error {
	prompt := buildPrompt(question, chunks)

	stream := c.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeSonnet4_6,
		MaxTokens: 2048,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})

	for stream.Next() {
		event := stream.Current()
		if event.Type == "content_block_delta" {
			delta := event.AsContentBlockDelta()
			if delta.Delta.Type == "text_delta" {
				fmt.Fprint(out, delta.Delta.Text)
			}
		}
	}
	// Ensure the response ends with a newline
	fmt.Fprintln(out)
	return stream.Err()
}

func buildPrompt(question string, chunks []ContextChunk) string {
	var sb strings.Builder

	sb.WriteString("Tu es un assistant expert sur la plateforme Formance (stack open-source de finance embarquée).\n")
	sb.WriteString("Réponds à la question en te basant uniquement sur les extraits de documentation fournis ci-dessous.\n")
	sb.WriteString("Si la réponse n'est pas dans la documentation, indique-le clairement plutôt que d'inventer.\n")
	sb.WriteString("Cite les sources pertinentes à la fin de ta réponse.\n\n")
	sb.WriteString("=== DOCUMENTATION ===\n\n")

	for i, c := range chunks {
		fmt.Fprintf(&sb, "[%d] %s", i+1, c.SourceFile)
		if c.Section != "" {
			fmt.Fprintf(&sb, " › %s", c.Section)
		}
		sb.WriteString("\n")
		sb.WriteString(c.Content)
		sb.WriteString("\n\n")
	}

	sb.WriteString("=== FIN DE LA DOCUMENTATION ===\n\n")
	fmt.Fprintf(&sb, "Question : %s\n\nRéponse :", question)

	return sb.String()
}
