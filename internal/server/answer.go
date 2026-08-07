package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/llm"
	"github.com/celestingeniaut/docs-rag/internal/store"
)

// noResultsMessage is what every transport returns when retrieval comes back
// empty, kept in one place so the web UI and the MCP tool word it the same way.
const noResultsMessage = "No relevant passage found in the documentation."

// Repo paths map to public documentation URLs. Only files under content/pages/
// are addressable; content/fragments/ holds partials included by other pages,
// which have no standalone URL.
const (
	docsPagesPrefix = "apps/docs/content/pages/"
	docsBaseURL     = "https://docs.formance.com/"
)

// sourceURL returns the public documentation URL for a source file, or "" when
// the file is not a standalone page.
//
// The section, when it is a heading, becomes an anchor. Pages here run long —
// the answer often rests on one heading out of twenty — so landing on the page
// and leaving the reader to find the passage wastes most of what a citation is
// for. A wrong anchor is a cheap failure: the browser ignores it and shows the
// top of the page, exactly what we had before.
func sourceURL(file, section string) string {
	if !strings.HasPrefix(file, docsPagesPrefix) {
		return ""
	}
	p := strings.TrimPrefix(file, docsPagesPrefix)
	p = strings.TrimSuffix(p, ".mdx")
	p = strings.TrimSuffix(p, ".md")
	p = strings.TrimSuffix(p, "/index")
	if p == "" {
		return ""
	}
	if a := anchor(section); a != "" {
		return docsBaseURL + p + "#" + a
	}
	return docsBaseURL + p
}

// anchor slugifies a heading the way the docs site generates its heading ids:
// lowercased, punctuation dropped, spaces joined with hyphens.
//
// Returns "" for a section that is not a heading. Generated datasets label
// their chunks "api.operations › v2CreateLedger", which is an identifier rather
// than something the page renders — and those files have no public URL anyway,
// so this is belt and braces.
func anchor(section string) string {
	if section == "" || strings.Contains(section, "›") {
		return ""
	}
	var b strings.Builder
	lastHyphen := true // suppress a leading hyphen
	for _, r := range strings.ToLower(section) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case r == ' ' || r == '-' || r == '_':
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
		// Everything else — backticks, brackets, accents, punctuation — is dropped,
		// which is what the site's own slugifier does.
	}
	return strings.TrimSuffix(b.String(), "-")
}

// renderSources formats a citation list to append to a generated answer.
//
// The MCP tool also returns sources as structured data, but an agent relaying
// "the answer" drops anything that is not part of the answer string: local
// testing showed citation markers and source links being stripped even with an
// explicit instruction to keep them. Folding the list into the text makes
// faithful relay the path of least resistance rather than a matter of goodwill.
//
// Entries are never deduplicated: index i must stay the [i+1] marker the model
// used, and one page can legitimately be cited twice for different sections.
func renderSources(sources []source) string {
	if len(sources) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n---\nSources:\n")
	for i, s := range sources {
		fmt.Fprintf(&b, "[%d] ", i+1)
		if s.Section != "" {
			fmt.Fprintf(&b, "%s — ", s.Section)
		}
		// Derived from the path rather than read from s.URL: pure and independent
		// of whether the caller populated that field.
		if u := sourceURL(s.File, s.Section); u != "" {
			b.WriteString(u)
		} else {
			// Fragments have no standalone page; the repo path is all we can cite.
			b.WriteString(s.File)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// stageErr names the pipeline stage that failed, so each transport can render
// the wording it wants while sharing one implementation.
type stageErr struct {
	Stage string
	Err   error
}

func (e *stageErr) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *stageErr) Unwrap() error { return e.Err }

// answerResult reports what one pipeline run produced.
type answerResult struct {
	// Sources are in prompt order: Sources[i] is the citation marked [i+1] in
	// the generated answer.
	Sources []source
	Status  string // "ok", "no_results" or "error"
}

// answer runs the full pipeline — translate, embed, search, generate — writing
// the generated text to out.
//
// Every surface that answers a documentation question goes through here: the
// web UI passes an SSE writer, the MCP tool passes a buffer. Because the prompt,
// filters, retrieval parameters and model are shared, both return the same
// answer for the same input. Do not reimplement this pipeline elsewhere.
func (s *srv) answer(ctx context.Context, req askReq, out io.Writer) (answerResult, error) {
	// Translate to English before embedding so the query matches the English docs.
	queryEn, err := s.llm.Translate(ctx, req.Question)
	if err != nil {
		queryEn = req.Question // graceful fallback
	}

	vec, err := s.emb.Embed(ctx, queryEn)
	if err != nil {
		return answerResult{Status: "error"}, &stageErr{Stage: "Embedding failed", Err: err}
	}

	// Build search filter from request parameters.
	filter := store.SearchFilter{License: req.License, Deployment: req.Deployment, Dimensions: req.dimensions()}
	if filter.License == "" {
		filter.License = "ee"
	}
	if filter.Deployment == "" {
		filter.Deployment = "both"
	}
	if req.Version != "" {
		// "3.2" → 32, "3.0" → 30, etc.
		if parts := strings.SplitN(req.Version, ".", 2); len(parts) == 2 {
			major, _ := strconv.Atoi(parts[0])
			minor, _ := strconv.Atoi(parts[1])
			filter.VersionNum = int16(major*10 + minor)
		}
	}

	// Fetch more candidates than needed, then apply a diversity filter so no
	// single file monopolises the context window.
	rawChunks, err := s.store.Search(ctx, vec, queryEn, req.TopK*2, 0.25, filter)
	if err != nil {
		return answerResult{Status: "error"}, &stageErr{Stage: "Search failed", Err: err}
	}

	seenFile := map[string]int{}
	var chunks []store.DocChunk
	for _, c := range rawChunks {
		if seenFile[c.SourceFile] >= 2 {
			continue
		}
		seenFile[c.SourceFile]++
		chunks = append(chunks, c)
		if len(chunks) >= req.TopK {
			break
		}
	}
	for _, c := range chunks {
		log.Printf("retrieval score=%.3f  section=%-30s  file=%s", c.Score, c.Section, c.SourceFile)
	}
	if len(chunks) == 0 {
		return answerResult{Status: "no_results"}, nil
	}

	ctxChunks := make([]llm.ContextChunk, len(chunks))
	sources := make([]source, len(chunks))
	for i, c := range chunks {
		ctxChunks[i] = llm.ContextChunk{SourceFile: c.SourceFile, Section: c.Section, Content: c.Content}
		sources[i] = source{File: c.SourceFile, Section: c.Section, URL: sourceURL(c.SourceFile, c.Section)}
	}

	filterCtx := llm.FilterContext{License: req.License, Deployment: req.Deployment, Version: req.Version}
	if req.License == "community" {
		filterCtx.EEFeatures = s.enterpriseFeatures(ctx)
	}
	// Warn about experimental features only when one was actually retrieved, so
	// the rule costs nothing on the vast majority of questions.
	for _, c := range chunks {
		if c.Badge == "experimental" {
			filterCtx.Experimental = true
			break
		}
	}
	// Generate from the ORIGINAL question (not the English translation): the
	// translation is only for retrieval (matching the English docs). Passing the
	// user's own wording makes the model answer in the user's language.
	if err := s.llm.Ask(ctx, req.Question, req.History, ctxChunks, filterCtx, out); err != nil {
		return answerResult{Sources: sources, Status: "error"}, &stageErr{Stage: "LLM error", Err: err}
	}

	return answerResult{Sources: sources, Status: "ok"}, nil
}

// normalise applies the same caps and defaults on every transport.
func (r *askReq) normalise() {
	if len(r.History) > maxHistoryLen {
		r.History = r.History[len(r.History)-maxHistoryLen:]
	}
	if r.TopK <= 0 || r.TopK > 30 {
		r.TopK = 15
	}
}
