package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// askDocsDescription is what an agent reads to decide whether to call the tool
// and how to fill its arguments. It is the highest-leverage text in this file:
// an agent that never calls the tool, or relays its answer loosely, is almost
// always a wording problem here rather than a code problem.
const askDocsDescription = `Answer a question about the Formance platform from its official documentation.

Use this for any question about how Formance works: Ledger and Numscript, API endpoints and parameters, the fctl CLI, Payments and Connectivity, Wallets, Flows, Reconciliation, configuration, versions and changelogs.

The answer is grounded exclusively in the documentation and carries [n] citation markers matching the returned sources. Relay it verbatim: do not reword, summarise, extend or correct it, and keep the source links. If the answer is incomplete, say so explicitly rather than filling the gap from your own knowledge.

Set license to "community" whenever the customer is on Community edition — the answer then flags Enterprise-only features instead of presenting them as available. Set sdk when the customer works in a known language, so examples come back in it rather than mixed. Ask a full question rather than keywords; the retrieval is semantic, not lexical.`

// askDocsInput is the tool's parameter schema. Each jsonschema tag is the
// description the model sees for that argument.
type askDocsInput struct {
	Question   string `json:"question" jsonschema:"the question to answer, in any language; the answer comes back in the same language"`
	Version    string `json:"version,omitempty" jsonschema:"Formance Stack version to restrict the documentation to, e.g. 3.2; omit when unknown"`
	Deployment string `json:"deployment,omitempty" jsonschema:"deployment model: cloud, self-hosted or both; omit when unknown"`
	License    string `json:"license,omitempty" jsonschema:"customer license: ee for Enterprise or community; set it whenever known so Enterprise-only features are flagged correctly"`
	Numscript  string `json:"numscript,omitempty" jsonschema:"Numscript interpreter in use: new or legacy; experimental language features exist only on the new interpreter"`
	SDK        string `json:"sdk,omitempty" jsonschema:"SDK the customer uses: typescript, go, python, java, csharp or php; set it to get examples in their language instead of a mix"`
}

// docSource is one cited documentation page.
type docSource struct {
	Citation int    `json:"citation" jsonschema:"the [n] marker used for this source in the answer"`
	File     string `json:"file" jsonschema:"path of the file in the documentation repository"`
	Section  string `json:"section,omitempty" jsonschema:"section heading within the page"`
	URL      string `json:"url,omitempty" jsonschema:"public documentation URL; empty for fragments that have no standalone page"`
}

// askDocsOutput is the tool's result schema.
type askDocsOutput struct {
	Answer  string      `json:"answer" jsonschema:"the answer, grounded in the documentation; relay verbatim"`
	Sources []docSource `json:"sources" jsonschema:"sources cited in the answer, in citation order"`
}

// askDocs runs the same pipeline as the web UI, buffering the generated answer
// instead of streaming it: MCP returns one result, not a stream.
func (s *srv) askDocs(ctx context.Context, _ *mcp.CallToolRequest, in askDocsInput) (*mcp.CallToolResult, askDocsOutput, error) {
	question := strings.TrimSpace(in.Question)
	if question == "" {
		return nil, askDocsOutput{}, fmt.Errorf("question is required")
	}
	if len(question) > maxQuestionLen {
		return nil, askDocsOutput{}, fmt.Errorf("question too long (max %d characters)", maxQuestionLen)
	}

	req := askReq{
		Question:   question,
		Version:    in.Version,
		Deployment: in.Deployment,
		License:    in.License,
		Numscript:  in.Numscript,
		SDK:        in.SDK,
	}
	req.normalise()

	start := time.Now()
	var buf bytes.Buffer
	res, err := s.answer(ctx, req, &buf)

	// Log MCP traffic alongside web traffic so /api/stats covers both.
	go func() {
		if err := s.store.InsertRequestLog(
			context.Background(), question,
			int(time.Since(start).Milliseconds()), len(res.Sources), res.Status,
		); err != nil {
			log.Printf("request log insert: %v", err)
		}
	}()

	if err != nil {
		return nil, askDocsOutput{}, err
	}
	if res.Status == "no_results" {
		return nil, askDocsOutput{Answer: noResultsMessage}, nil
	}

	// Sources are appended to the answer text as well as returned structurally —
	// see renderSources for why.
	out := askDocsOutput{
		Answer:  buf.String() + renderSources(res.Sources),
		Sources: make([]docSource, len(res.Sources)),
	}
	for i, src := range res.Sources {
		out.Sources[i] = docSource{
			Citation: i + 1, // the prompt numbers excerpts [1], [2], … in this order
			File:     src.File,
			Section:  src.Section,
			URL:      src.URL,
		}
	}
	return nil, out, nil
}

// mcpHandler serves the Model Context Protocol endpoint over streamable HTTP.
//
// Deliberately not wrapped in withRateLimit: that limiter exists to stop an
// anonymous browser from hammering /api/ask, and it counts per client IP. Agent
// traffic arrives from a single gateway address, so the 5-per-minute budget
// would be shared across every user of that gateway. Access here is already
// restricted upstream, by network policy and by the agent's own allowlist.
func (s *srv) mcpHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "docs-rag",
		Version: "v1",
	}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ask_formance_docs",
		Description: askDocsDescription,
	}, s.askDocs)
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}
