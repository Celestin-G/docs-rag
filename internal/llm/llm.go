package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const defaultHost = "http://localhost:11434"
const model = "qwen2.5:32b"

// Client calls the Ollama /api/chat endpoint with streaming.
type Client struct {
	host   string
	client *http.Client
}

// New creates a Client. Reads OLLAMA_HOST from the environment (default: http://localhost:11434).
func New() *Client {
	host := os.Getenv("OLLAMA_HOST")
	if host == "" {
		host = defaultHost
	}
	return &Client{host: host, client: &http.Client{}}
}

// ContextChunk is a retrieved doc chunk used as RAG context.
type ContextChunk struct {
	SourceFile string
	Section    string
	Content    string
}

// FilterContext describes the active user-selected filters.
type FilterContext struct {
	License    string // "ee" or "community"
	Deployment string // "cloud", "self-hosted", "both"
	Version    string // e.g. "3.2"
}

// Message is a single turn in the conversation history.
type Message struct {
	Role    string `json:"role"`    // "user" or "assistant"
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatChunk struct {
	Message Message `json:"message"`
	Done    bool    `json:"done"`
}

// Translate translates text to English. Returns the original text unchanged if
// already in English or if the call fails (graceful degradation).
func (c *Client) Translate(ctx context.Context, text string) (string, error) {
	messages := []Message{
		{
			Role:    "system",
			Content: "Translate the following text to English. If it is already in English, return it unchanged. Output only the translation — no explanation, no quotes, no extra text.",
		},
		{Role: "user", Content: text},
	}
	body, err := json.Marshal(chatRequest{Model: model, Messages: messages, Stream: false})
	if err != nil {
		return text, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return text, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return text, err
	}
	defer resp.Body.Close()

	var result struct {
		Message Message `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return text, err
	}
	if t := strings.TrimSpace(result.Message.Content); t != "" {
		return t, nil
	}
	return text, nil
}

// Ask streams a response. history contains all previous turns; chunks are the
// RAG context retrieved for the current question.
func (c *Client) Ask(ctx context.Context, question string, history []Message, chunks []ContextChunk, filter FilterContext, out io.Writer) error {
	messages := make([]Message, 0, len(history)+2)

	// System message: role + RAG context for this turn
	messages = append(messages, Message{
		Role:    "system",
		Content: buildSystem(chunks, filter),
	})

	// Full conversation history (all previous user+assistant turns)
	messages = append(messages, history...)

	// Current question
	messages = append(messages, Message{Role: "user", Content: question})

	body, err := json.Marshal(chatRequest{Model: model, Messages: messages, Stream: true})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.host+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama chat: HTTP %d", resp.StatusCode)
	}

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var chunk chatChunk
		if err := json.Unmarshal(line, &chunk); err != nil {
			continue
		}
		fmt.Fprint(out, chunk.Message.Content)
		if chunk.Done {
			break
		}
	}
	fmt.Fprintln(out)
	return scanner.Err()
}

func buildSystem(chunks []ContextChunk, filter FilterContext) string {
	var sb strings.Builder

	sb.WriteString("You are a technical assistant for the Formance platform.\n\n")

	// Inject active filter context so the LLM knows what is and isn't available.
	sb.WriteString("## Active documentation filter\n")
	if filter.Version != "" {
		fmt.Fprintf(&sb, "- Stack version: v%s\n", filter.Version)
	}
	if filter.Deployment != "" && filter.Deployment != "both" {
		fmt.Fprintf(&sb, "- Deployment: %s\n", filter.Deployment)
	}
	if filter.License == "community" {
		sb.WriteString("- License: **Community edition**\n")
		sb.WriteString("  The following Formance modules are EXCLUSIVELY for Enterprise (EE) customers and have been removed from the search results:\n")
		sb.WriteString("  Wallets module, Flows module, Reconciliation module, SSO, RBAC, Audit Logs.\n")
		sb.WriteString("  CRITICAL RULE: If the user's question is about any of these EE features — even indirectly (e.g. 'add funds to my wallet/portefeuille', 'create a flow', 'reconcile payments') — you MUST answer that this feature requires an Enterprise (EE) license and is not available in Community edition. Do NOT use PSP connector pages, Ledger pages, or any other tangentially related content to construct an answer for an EE-only question. Say clearly: this is an EE feature.\n")
	} else {
		sb.WriteString("- License: Enterprise (EE) — all features are available.\n")
	}
	sb.WriteString("\n")

	sb.WriteString("## Technical facts — must come exclusively from the documentation excerpts below\n")
	sb.WriteString("- API endpoints, HTTP methods, URL paths\n")
	sb.WriteString("- Parameter names, types, and accepted values\n")
	sb.WriteString("- CLI commands and their flags (copy verbatim — never add flags not shown in the docs)\n")
	sb.WriteString("- Configuration field names, defaults, and enum values\n")
	sb.WriteString("- Feature names, capability names, status values, metadata keys\n")
	sb.WriteString("- Any numeric value, asset format, or connector-specific behavior\n")
	sb.WriteString("If a specific technical detail is not in the excerpts, say so clearly — do not guess or infer it.\n\n")

	sb.WriteString("## Synthesis and guidance — you may construct from documented facts\n")
	sb.WriteString("- Step-by-step guides that sequence documented operations\n")
	sb.WriteString("- Architecture recommendations based on documented patterns\n")
	sb.WriteString("- Explanations that connect multiple documented concepts\n")
	sb.WriteString("- Suggested approaches when the user asks 'how do I set up X'\n\n")

	sb.WriteString("## Hard rules\n")
	sb.WriteString("- Never mix capabilities or behaviors across connectors — each connector has its own documented spec.\n")
	sb.WriteString("- Never invent code, YAML, or JSON that is not present verbatim in the excerpts.\n")
	sb.WriteString("- Always respond in the same language as the user's question.\n")
	sb.WriteString("- If the excerpts below do not contain information that directly answers the question, say so explicitly. Do NOT attempt to reconstruct an answer by combining loosely related excerpts. A clear 'I don't have documentation on this in the current filter context' is better than a synthesized guess.\n")
	sb.WriteString("- If the question is about a feature that appears to be an Enterprise (EE) edition feature (Wallets, Flows, Reconciliation, SSO, RBAC, Audit Logs) but no relevant excerpts are present, explicitly tell the user that this feature may not be available in their current license or filter context.\n\n")

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

	sb.WriteString("=== FIN DE LA DOCUMENTATION ===")
	return sb.String()
}

