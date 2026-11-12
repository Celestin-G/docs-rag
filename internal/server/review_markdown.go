package server

import (
	"bytes"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// reviewMD renders the bot's markdown drafts to HTML for the review page.
//
// Safety model, from the outside in:
//   - html.WithUnsafe() is deliberately NOT set, so goldmark drops raw HTML
//     (<script>, <img onerror=...>, ...) and replaces it with an HTML comment.
//   - safeLinks (below) then enforces an allowlist of link schemes, turns
//     images into plain links (a draft must not make the reviewer's browser
//     fetch arbitrary URLs), and makes every link open in a new tab with
//     rel="noopener noreferrer".
//   - The page's Content-Security-Policy (see handleReviewPage) is the last
//     line of defence: no inline script, img-src 'none'.
//
// The TeamReply and Question fields are never rendered as markdown or HTML:
// the client inserts them with textContent.
var reviewMD = goldmark.New(
	goldmark.WithExtensions(extension.Strikethrough, extension.Table),
	goldmark.WithParserOptions(
		parser.WithASTTransformers(util.Prioritized(safeLinks{}, 100)),
	),
)

// renderReviewMarkdown converts a markdown draft to sanitised HTML.
func renderReviewMarkdown(src string) (string, error) {
	var buf bytes.Buffer
	if err := reviewMD.Convert([]byte(src), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// safeLinks is a goldmark AST transformer applying the link rules above.
type safeLinks struct{}

func (safeLinks) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	var images []*ast.Image
	var links []*ast.Link
	var autos []*ast.AutoLink
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Image:
			images = append(images, v)
		case *ast.Link:
			links = append(links, v)
		case *ast.AutoLink:
			autos = append(autos, v)
		}
		return ast.WalkContinue, nil
	})

	// Images become links to the same target, keeping the alt text as label.
	for _, img := range images {
		l := ast.NewLink()
		l.Destination = img.Destination
		l.Title = img.Title
		for c := img.FirstChild(); c != nil; c = img.FirstChild() {
			l.AppendChild(l, c)
		}
		if l.ChildCount() == 0 {
			l.AppendChild(l, ast.NewString(img.Destination))
		}
		img.Parent().ReplaceChild(img.Parent(), img, l)
		links = append(links, l)
	}

	for _, l := range links {
		if !allowedURL(string(l.Destination)) {
			l.Destination = []byte("#")
			l.Title = nil
		}
		markExternal(l)
	}
	for _, a := range autos {
		if !allowedURL(string(a.URL(reader.Source()))) {
			// Not a link we are willing to emit: show it as inert text.
			a.Parent().ReplaceChild(a.Parent(), a, ast.NewString(a.Label(reader.Source())))
			continue
		}
		markExternal(a)
	}
}

func markExternal(n ast.Node) {
	n.SetAttributeString("target", []byte("_blank"))
	n.SetAttributeString("rel", []byte("noopener noreferrer"))
}

// allowedURL reports whether a link destination may be emitted as an href:
// http(s) and mailto, or a scheme-less (relative or fragment) reference.
// Everything else (javascript:, data:, vbscript:, file:, ...) is refused.
func allowedURL(u string) bool {
	u = strings.TrimSpace(u)
	// Browsers strip tabs and newlines inside a URL, so "java\tscript:" is a
	// scheme to them: drop control characters before looking for one.
	u = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, u)
	i := strings.IndexAny(u, ":/?#")
	if i < 0 || u[i] != ':' {
		return true // no scheme: relative reference
	}
	switch strings.ToLower(u[:i]) {
	case "http", "https", "mailto":
		return true
	}
	return false
}
