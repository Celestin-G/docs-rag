package kbpublish

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/celestingeniaut/docs-rag/internal/chunker"
	"github.com/celestingeniaut/docs-rag/internal/docsource"
)

// maxFileBytes is the budget of one knowledge-base file as uploaded: breadcrumb,
// "Applies to:" header and body together. A longer chunk is split again, the
// breadcrumb and header repeated in each piece. The Bedrock chunker is semantic
// with a 512-token ceiling; about 1500 bytes keeps a file under it in most cases
// (not guaranteed: a long breadcrumb or unbreakable line can exceed it).
const maxFileBytes = 1500

// minPartBudget keeps a very long breadcrumb from squeezing the body to nothing.
const minPartBudget = 400

// DefaultExcludes are left out unless --no-default-excludes is passed: the raw
// OpenAPI dumps and the JSON-schema dump. The API is published in readable form
// by the api.operations and membership.operations files.
var DefaultExcludes = []string{
	"apps/docs/content/data/generated/stack.oas.json",
	"apps/docs/content/data/generated/stack.oas.enriched.json",
	"apps/docs/content/data/generated/stack.oas.prose.json",
	"apps/docs/content/data/generated/membership.oas.json",
	"apps/docs/content/data/generated/api.schemas.json",
}

// DefaultDatasetExcludes are datasets left out of generated files by default: the
// by-fingerprint index duplicates the snippets of the same numscript.snippets.json.
var DefaultDatasetExcludes = []string{
	"cookbook.numscript.byFingerprint",
}

// SourceFile is one documentation file read from the docs checkout.
type SourceFile struct {
	Rel     string // slash path relative to the docs root
	Content []byte
}

// Options configure Build.
type Options struct {
	Table    *StackTable
	Dates    DateSource
	Excludes []string // globs on repo-relative paths
	// DatasetExcludes are globs on dataset names inside generated files (the
	// "dataset" of "dataset › entry"): those chunks are left out.
	DatasetExcludes []string
}

// DatasetStat counts the knowledge-base files of one generated dataset.
type DatasetStat struct {
	Files int
	Bytes int // body plus sidecar
}

// Report is what Build observed, for the dry-run output.
type Report struct {
	FilesByKind map[string]int
	Excluded    int
	// DatasetExcluded counts the chunks left out per excluded dataset.
	DatasetExcluded map[string]int
	Chunks          int
	KBFiles         int
	ResplitChunks   int // chunks that became more than one file
	ExtraParts      int // files beyond the first of those chunks
	ByProduct       map[string]int
	ByDataset       map[string]DatasetStat // "<source file> <dataset>"
	Headers         int                    // files with an "Applies to:" line
	WhenNotes       int
	DocCaseNotes    int
	Widened         []Widening
	// Excluded*: ranges no Stack release ships. Entries are deduplicated by file
	// and bound; the counts are chunks (page-level) and blocks (inline).
	Unshipped       []Exclusion
	UnshippedChunks int
	UnshippedBlocks int
	// Gaps: ranges that overlap non-adjacent Stack releases; converted to a
	// superset (see Conversion.NonContiguous).
	Gaps            []Widening
	URLFallbacks    []string
	AnchorMisses    []string
	Unhandled       []string
	Empty           []string
	ParseFailures   []string
	DateFallbacks   int
	MaxSidecarBytes int
	MaxSidecarKey   string
	// TokenResplitChunks counts chunks cut into more files than the byte budget
	// alone would give, because of the estimated-token budget; TokenExtraParts is
	// the number of files that added.
	TokenResplitChunks int
	TokenExtraParts    int
	MaxFileTokens      int // largest estimated token count of any file
	MaxFileTokensKey   string
}

// Plan is the result of Build: the files to publish and everything worth
// reporting. Errors is non-empty when validation failed; nothing may be sent to
// AWS then.
type Plan struct {
	Files  []File
	Report Report
	Errors []string
}

// Build maps the documentation files to knowledge-base files.
func Build(sources []SourceFile, opts Options) *Plan {
	sort.Slice(sources, func(i, j int) bool { return sources[i].Rel < sources[j].Rel })
	refs := buildRefs(sources)

	p := &Plan{Report: Report{
		FilesByKind:     map[string]int{},
		ByProduct:       map[string]int{},
		ByDataset:       map[string]DatasetStat{},
		DatasetExcluded: map[string]int{},
	}}
	rep := &p.Report
	owner := map[string]string{} // key -> source file
	seenWiden := map[string]bool{}

	for _, src := range sources {
		kind := docsource.Kind(src.Rel)
		if kind == "" {
			continue
		}
		if excluded(src.Rel, opts.Excludes) {
			rep.Excluded++
			continue
		}
		rep.FilesByKind[kind]++

		chunks, inl, err := parseSource(src, kind, opts.Table)
		if err != nil {
			rep.ParseFailures = append(rep.ParseFailures, fmt.Sprintf("%s: %v", src.Rel, err))
			continue
		}
		rep.WhenNotes += inl.WhenNotes
		rep.DocCaseNotes += inl.DocCaseNotes
		rep.Unhandled = append(rep.Unhandled, inl.Unhandled...)
		for _, w := range inl.Widened {
			addWidening(rep, seenWiden, w)
		}
		for _, g := range inl.Gaps {
			addGap(rep, seenWiden, g)
		}
		for _, x := range inl.Excluded {
			addExclusion(rep, seenWiden, x)
		}
		rep.UnshippedBlocks += inl.Dropped
		if len(chunks) == 0 {
			rep.Empty = append(rep.Empty, src.Rel)
			continue
		}
		rep.Chunks += len(chunks)

		fc := &fileContext{
			src: src, kind: kind, opts: opts, refs: refs, rep: rep,
			counts: map[string]int{}, leafSeen: map[string]int{}, seqAnchor: map[int]string{},
		}
		if kind == docsource.KindPage {
			fc.headings = pageHeadings(string(src.Content))
		}
		if _, own := opts.Dates.Date(src.Rel); !own {
			rep.DateFallbacks++
		}
		for _, c := range chunks {
			for _, f := range fc.files(c, p, seenWiden) {
				if prev, dup := owner[f.file.Key]; dup {
					p.Errors = append(p.Errors, fmt.Sprintf("duplicate key %s: %s and %s", f.file.Key, prev, src.Rel))
					continue
				}
				owner[f.file.Key] = src.Rel
				p.Files = append(p.Files, f.file)
				rep.KBFiles++
				rep.ByProduct[f.product]++
				if f.header {
					rep.Headers++
				}
				if f.dataset != "" {
					k := src.Rel + " " + f.dataset
					st := rep.ByDataset[k]
					st.Files++
					st.Bytes += len(f.file.Body) + len(f.file.Sidecar)
					rep.ByDataset[k] = st
				}
				if f.tokens > rep.MaxFileTokens || (f.tokens == rep.MaxFileTokens && f.file.Key < rep.MaxFileTokensKey) {
					rep.MaxFileTokens, rep.MaxFileTokensKey = f.tokens, f.file.Key
				}
				if n := f.sidecarBytes; n > rep.MaxSidecarBytes {
					rep.MaxSidecarBytes, rep.MaxSidecarKey = n, f.file.Key
				}
			}
		}
	}

	for _, list := range [][]Widening{rep.Widened, rep.Unshipped, rep.Gaps} {
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.File != b.File {
				return a.File < b.File
			}
			if a.Inline != b.Inline {
				return !a.Inline
			}
			if a.Component != b.Component {
				return a.Component < b.Component
			}
			return a.Bound < b.Bound
		})
	}
	rep.URLFallbacks = collapse(rep.URLFallbacks)
	rep.AnchorMisses = collapse(rep.AnchorMisses)
	sort.Strings(rep.Unhandled)
	sort.Strings(p.Errors)
	sort.Slice(p.Files, func(i, j int) bool { return p.Files[i].Key < p.Files[j].Key })
	return p
}

// collapse sorts lines and folds repeats into "line (xN)": a chunk-level event
// reported once per chunk would drown the list in copies.
func collapse(lines []string) []string {
	counts := map[string]int{}
	for _, l := range lines {
		counts[l]++
	}
	out := make([]string, 0, len(counts))
	for _, l := range sortedKeys(counts) {
		if n := counts[l]; n > 1 {
			l = fmt.Sprintf("%s (x%d)", l, n)
		}
		out = append(out, l)
	}
	return out
}

func addWidening(rep *Report, seen map[string]bool, w Widening) {
	id := fmt.Sprintf("w|%s|%v|%s|%s", w.File, w.Inline, w.Component, w.Bound)
	if seen[id] {
		return
	}
	seen[id] = true
	rep.Widened = append(rep.Widened, w)
}

func addGap(rep *Report, seen map[string]bool, g Widening) {
	id := fmt.Sprintf("g|%s|%v|%s|%s", g.File, g.Inline, g.Component, g.Bound)
	if seen[id] {
		return
	}
	seen[id] = true
	rep.Gaps = append(rep.Gaps, g)
}

func addExclusion(rep *Report, seen map[string]bool, x Exclusion) {
	id := fmt.Sprintf("x|%s|%v|%s|%s", x.File, x.Inline, x.Component, x.Bound)
	if seen[id] {
		return
	}
	seen[id] = true
	rep.Unshipped = append(rep.Unshipped, x)
}

// parseSource chunks one file; a panic in the parser becomes an error.
func parseSource(src SourceFile, kind string, t *StackTable) (chunks []chunker.Chunk, inl InlineReport, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if kind == docsource.KindGenerated {
		chunks = chunker.ParseGeneratedJSON(src.Rel, src.Content)
		// The parser walks a Go map of datasets: put them in a fixed order so keys
		// and their -2/-3 suffixes cannot change between runs.
		sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].Section < chunks[j].Section })
		return chunks, inl, nil
	}
	rewritten, inl := RewriteInline(src.Rel, src.Content, t)
	return chunker.ParseFile(src.Rel, rewritten), inl, nil
}

type builtFile struct {
	file         File
	product      string
	dataset      string
	header       bool
	sidecarBytes int
	tokens       int
}

type fileContext struct {
	src      SourceFile
	kind     string
	opts     Options
	refs     *refIndex
	rep      *Report
	headings []heading

	counts    map[string]int // base key -> times used
	leafSeen  map[string]int // leaf heading -> sections seen
	seqAnchor map[int]string // SectionSeq -> anchor ("" none)
	lastSeq   int
	started   bool
}

// files maps one chunk to one or more knowledge-base files.
func (fc *fileContext) files(c chunker.Chunk, p *Plan, seenWiden map[string]bool) []builtFile {
	rel := fc.src.Rel
	conv := fc.opts.Table.Convert(c.VersionComponent, c.VersionMinRaw, c.VersionBelowRaw)

	product, base, dataset, entry := fc.place(c)
	if fc.kind == docsource.KindGenerated && excluded(dataset, fc.opts.DatasetExcludes) {
		fc.rep.DatasetExcluded[dataset]++
		return nil
	}
	url := fc.sourceURL(c, dataset, entry)
	// The key counter and anchor scan advance for every chunk, excluded or not, so
	// excluding one never renumbers the -2/-3 keys or anchors of the next.
	n := fc.counts[base]
	fc.counts[base]++
	if conv.Empty {
		addExclusion(fc.rep, seenWiden, widening(rel, false, conv))
		fc.rep.UnshippedChunks++
		return nil
	}
	if conv.Widened() {
		addWidening(fc.rep, seenWiden, widening(rel, false, conv))
	}
	if conv.NonContiguous {
		addGap(fc.rep, seenWiden, widening(rel, false, conv))
	}
	updated, _ := fc.opts.Dates.Date(rel)

	license := "any"
	switch {
	case c.Badge == "enterprise" || contains(c.Visibility["license"], "ee"):
		license = "ee"
	case contains(c.Visibility["license"], "oss"):
		license = "oss"
	}
	deployment := c.Deployment
	if deployment == "" || deployment == "both" {
		deployment = "any"
	}
	attrs := Attrs{
		"doc_type":          "doc",
		"product":           product,
		"version_min":       conv.Min,
		"version_below":     conv.Below,
		"version_component": versionComponent(c.VersionComponent),
		"deployment":        deployment,
		"license":           license,
		"client_id":         "none",
		"language":          "en",
		"source_url":        url,
		"updated_at":        updated,
	}
	aud := Audience{License: license, Experiment: c.Badge == "experimental"}
	if aud.License == "any" {
		aud.License = ""
	}
	if deployment != "any" {
		aud.Deployment = deployment
	}
	if v := c.Visibility["numscript"]; len(v) == 1 && (v[0] == "new" || v[0] == "legacy") {
		aud.Numscript = v[0]
	}
	header := Header(conv, aud)

	breadcrumb, body := c.Breadcrumb, c.Body
	if c.Breadcrumb == "" && c.Body == "" { // generated data
		body = c.Content
		if first, _, _ := strings.Cut(body, "\n"); first != c.Section {
			breadcrumb = c.Section
		}
	}
	prefix := breadcrumb
	if header != "" {
		if prefix != "" {
			prefix += "\n"
		}
		prefix += header
	}

	bodies, byteParts := splitForBudgets(prefix, body)
	if len(bodies) > 1 {
		fc.rep.ResplitChunks++
		fc.rep.ExtraParts += len(bodies) - 1
	}
	if len(bodies) > byteParts {
		fc.rep.TokenResplitChunks++
		fc.rep.TokenExtraParts += len(bodies) - byteParts
	}

	// The same base key twice on a page gets -2, -3 in document order.
	if n > 0 {
		base = fmt.Sprintf("%s-%d", base, n+1)
	}

	var out []builtFile
	for i, part := range bodies {
		key := base
		if i > 0 {
			key = fmt.Sprintf("%s-p%d", base, i+1)
		}
		key += ".md"
		text := part
		if prefix != "" {
			text = prefix + "\n\n" + part
		}
		f := NewFile(key, text, attrs)
		for _, e := range ValidateKey(key) {
			p.Errors = append(p.Errors, fmt.Sprintf("%s: %s", rel, e))
		}
		for _, e := range attrs.Validate() {
			p.Errors = append(p.Errors, fmt.Sprintf("%s (%s): %s", rel, key, e))
		}
		tokens := estimateTokens(text)
		for _, e := range ValidateFileTokens(text) {
			p.Errors = append(p.Errors, fmt.Sprintf("%s (%s): %s", rel, key, e))
		}
		if strings.TrimSpace(part) == "" {
			p.Errors = append(p.Errors, fmt.Sprintf("%s (%s): empty body", rel, key))
		}
		out = append(out, builtFile{file: f, product: product, dataset: dataset, header: header != "", sidecarBytes: attrs.attributesSize(), tokens: tokens})
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func versionComponent(c string) string {
	if c == "" {
		return "stack"
	}
	return strings.ToLower(c)
}

var generatedSep = " › "

// place decides the product and the key (without .md) of a chunk, and for
// generated data its dataset and entry id.
func (fc *fileContext) place(c chunker.Chunk) (product, base, dataset, entry string) {
	rel := fc.src.Rel
	section := "_intro"
	if c.Section != "" {
		section = keySlug(c.Section)
	}
	switch fc.kind {
	case docsource.KindGenerated:
		name := strings.TrimSuffix(path.Base(rel), ".json")
		product = generatedProduct(name)
		dataset, entry, _ = strings.Cut(c.Section, generatedSep)
		if entry == "" {
			entry = dataset
		}
		return product, fmt.Sprintf("docs/%s/_data/%s/%s", product, keySlug(dataset), keySlug(entry)), dataset, entry
	case docsource.KindFragment:
		sub := strings.TrimSuffix(strings.TrimPrefix(rel, "apps/docs/content/fragments/"), path.Ext(rel))
		product = fragmentProduct(sub)
		segs := keySegments(strings.ReplaceAll(sub, "@", "-"))
		return product, "docs/" + product + "/_fragments/" + strings.Join(segs, "/") + "/" + section, "", ""
	case docsource.KindNumscriptEngine:
		sub := strings.TrimSuffix(strings.TrimPrefix(rel, "packages/numscript-engine/"), path.Ext(rel))
		return "numscript", "docs/numscript/_engine/" + strings.Join(keySegments(sub), "/") + "/" + section, "", ""
	}
	product = productFromPath(c.PagePath)
	segs := keySegments(c.PagePath)
	if c.PagePath == "" {
		sub := strings.TrimSuffix(strings.TrimPrefix(rel, "apps/docs/content/pages/"), path.Ext(rel))
		segs = keySegments(sub)
	}
	if len(segs) == 0 {
		segs = []string{"_index"}
	}
	return product, "docs/" + product + "/" + strings.Join(segs, "/") + "/" + section, "", ""
}

var moduleProducts = map[string]string{
	"ledger": "ledger", "payments": "payments", "wallets": "wallets", "flows": "flows",
	"orchestration": "flows", "reconciliation": "reconciliation", "numscript": "numscript", "webhooks": "webhooks",
}

// productFromPath maps a site path to a product.
func productFromPath(p string) string {
	segs := strings.Split(strings.Trim(p, "/"), "/")
	switch segs[0] {
	case "modules":
		if len(segs) > 1 {
			if m, ok := moduleProducts[segs[1]]; ok {
				return m
			}
		}
	case "deploy", "manage", "getting-started", "stack-api-reference":
		return "stack"
	}
	return "other"
}

func fragmentProduct(sub string) string {
	first, _, _ := strings.Cut(sub, "/")
	if m, ok := moduleProducts[first]; ok {
		return m
	}
	return "other"
}

// generatedProduct maps a generated data file name to a product by its prefix.
func generatedProduct(name string) string {
	prefix, _, _ := strings.Cut(name, ".")
	switch prefix {
	case "ledger", "payments", "numscript":
		return prefix
	case "stack", "api", "platform", "operator", "infra", "otel", "sdks", "cloud", "audit":
		return "stack"
	}
	return "other"
}

// sourceURL builds the link the assistant cites for a chunk.
func (fc *fileContext) sourceURL(c chunker.Chunk, dataset, entry string) string {
	rel := fc.src.Rel
	fallback := func(why string) string {
		fc.rep.URLFallbacks = append(fc.rep.URLFallbacks, rel+": "+why)
		return siteURL + "/"
	}
	switch fc.kind {
	case docsource.KindPage:
		if c.PagePath == "" {
			return fallback("page has no frontmatter path")
		}
		u := siteURL + c.PagePath
		if len(u) > maxSourceURLLen || strings.Contains(u, "?") {
			return fallback("page url too long or has a query")
		}
		if a := fc.anchor(c); a != "" {
			if len(u)+1+len(a) <= maxSourceURLLen {
				return u + "#" + a
			}
			fc.rep.AnchorMisses = append(fc.rep.AnchorMisses, rel+": anchor dropped, url too long: "+c.Leaf)
		}
		return u
	case docsource.KindFragment:
		if pg := fc.refs.pageOfFragment(rel); pg != "" {
			return siteURL + pg
		}
		return fallback("fragment is not included by any page")
	case docsource.KindGenerated:
		if pg := fc.refs.pageOfDataset(dataset, entry); pg != "" {
			return siteURL + pg
		}
		name := path.Base(rel)
		switch {
		case strings.HasPrefix(name, "api.") || strings.HasPrefix(name, "stack.oas"):
			return siteURL + "/stack-api-reference"
		case strings.HasPrefix(name, "membership."):
			return siteURL + "/membership-api"
		}
		return fallback("no page shows dataset " + dataset)
	}
	return fallback("no page for this source")
}

// anchor returns the id of the leaf heading of the chunk's section, "" for text
// before the first heading or a heading the page scan could not match.
func (fc *fileContext) anchor(c chunker.Chunk) string {
	if c.Leaf == "" {
		return ""
	}
	if fc.started && c.SectionSeq == fc.lastSeq {
		return fc.seqAnchor[c.SectionSeq]
	}
	fc.started, fc.lastSeq = true, c.SectionSeq
	n := fc.leafSeen[c.Leaf]
	fc.leafSeen[c.Leaf]++
	seen := 0
	for _, h := range fc.headings {
		if (h.depth == 2 || h.depth == 3) && h.raw == c.Leaf {
			if seen == n {
				fc.seqAnchor[c.SectionSeq] = h.id
				return h.id
			}
			seen++
		}
	}
	fc.rep.AnchorMisses = append(fc.rep.AnchorMisses, fc.src.Rel+": no heading matches "+c.Leaf)
	fc.seqAnchor[c.SectionSeq] = ""
	return ""
}

// splitBody cuts text into parts of at most budget bytes on paragraph, then
// line, then word boundaries. A cut inside a fenced code block closes the fence
// at the end of the part and reopens it (same info string) at the start of the
// next, so no part has an unbalanced fence.
func splitBody(text string, budget int) []string {
	if !strings.Contains(text, "```") {
		return cutBody(text, budget)
	}
	// Room for the closing fence and the longest opening fence of the text.
	overhead := 4
	for line := range strings.SplitSeq(text, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "```") {
			overhead = max(overhead, len(t)+len("\n```")+1)
		}
	}
	parts := cutBody(text, max(budget-overhead, budget/2))
	var (
		out     []string
		opening string // the opening fence line while inside a fence
	)
	for i, part := range parts {
		if opening != "" {
			// The previous part closed the fence for us: a stray closing line at
			// the top of this one is dropped, anything else reopens it.
			first, rest, _ := strings.Cut(part, "\n")
			if isFenceClose(first, opening) {
				opening = ""
				part = strings.TrimSpace(rest)
				if part == "" {
					continue
				}
			} else {
				part = opening + "\n" + part
			}
		}
		for line := range strings.SplitSeq(part, "\n") {
			t := strings.TrimSpace(line)
			switch {
			case opening == "" && strings.HasPrefix(t, "```"):
				opening = t
			case opening != "" && isFenceClose(t, opening):
				opening = ""
			}
		}
		if !hasContent(part) {
			// Only fence lines: the cut fell right after an opening fence. The
			// next part reopens it, so this one would be an empty code block.
			continue
		}
		if opening != "" && i < len(parts)-1 {
			part += "\n" + fenceMarker(opening)
		}
		out = append(out, part)
	}
	return out
}

// hasContent reports whether part has a line that is neither blank nor a fence.
func hasContent(part string) bool {
	for line := range strings.SplitSeq(part, "\n") {
		if t := strings.TrimSpace(line); t != "" && !strings.HasPrefix(t, "```") {
			return true
		}
	}
	return false
}

// fenceMarker returns the run of backticks that opens a fence line.
func fenceMarker(open string) string {
	return open[:len(open)-len(strings.TrimLeft(open, "`"))]
}

// isFenceClose reports whether line closes the fence opened by open.
func isFenceClose(line, open string) bool {
	t := strings.TrimSpace(line)
	return t != "" && strings.Trim(t, "`") == "" && len(t) >= len(fenceMarker(open))
}

func cutBody(text string, budget int) []string {
	var parts []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			parts = append(parts, s)
		}
		cur.Reset()
	}
	add := func(unit, sep string) {
		if cur.Len() > 0 && cur.Len()+len(sep)+len(unit) > budget {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString(sep)
		}
		cur.WriteString(unit)
	}
	for para := range strings.SplitSeq(text, "\n\n") {
		if strings.TrimSpace(para) == "" {
			continue
		}
		if len(para) <= budget {
			add(para, "\n\n")
			continue
		}
		flush()
		for line := range strings.SplitSeq(para, "\n") {
			for _, piece := range cutLine(line, budget) {
				add(piece, "\n")
			}
		}
		flush()
	}
	flush()
	if len(parts) == 0 {
		return []string{text}
	}
	return parts
}

// cutLine splits one line longer than budget at spaces, or at a rune boundary
// when it has none.
func cutLine(line string, budget int) []string {
	var out []string
	for len(line) > budget {
		cut := budget
		if i := strings.LastIndexByte(line[:budget], ' '); i > budget/2 {
			cut = i
		}
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		out = append(out, strings.TrimRight(line[:cut], " "))
		line = strings.TrimLeft(line[cut:], " ")
	}
	return append(out, line)
}

// excluded reports whether rel matches any glob. A pattern without a slash also
// matches the file name alone.
func excluded(rel string, globs []string) bool {
	for _, g := range globs {
		if globMatch(g, rel) || (!strings.Contains(g, "/") && globMatch(g, path.Base(rel))) {
			return true
		}
	}
	return false
}

// globMatch matches a pattern where * stays within a path segment, ** crosses
// segments and ? is one non-slash character.
func globMatch(pattern, name string) bool {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(name)
}
