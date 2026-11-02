package kbpublish

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/docsource"
)

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PrintSummary writes the counts of a plan: what was read, what will be sent and
// what the conversion had to widen.
func (r *Report) PrintSummary(w io.Writer, table *StackTable) {
	if table != nil {
		fmt.Fprintf(w, "Stack releases read from schema.ts: %s\n\n", strings.Join(table.Releases(), ", "))
	}
	fmt.Fprintln(w, "Source files")
	for _, k := range []string{docsource.KindPage, docsource.KindFragment, docsource.KindGenerated, docsource.KindNumscriptEngine} {
		fmt.Fprintf(w, "  %-18s %6d\n", k, r.FilesByKind[k])
	}
	if r.Excluded > 0 {
		fmt.Fprintf(w, "  %-18s %6d\n", "excluded", r.Excluded)
	}
	if len(r.DatasetExcluded) > 0 {
		total := 0
		for _, n := range r.DatasetExcluded {
			total += n
		}
		fmt.Fprintf(w, "  %-18s %6d chunks of excluded datasets\n", "excluded datasets", total)
	}
	fmt.Fprintf(w, "\nChunks                  %6d\n", r.Chunks)
	fmt.Fprintf(w, "KB files                %6d\n", r.KBFiles)
	fmt.Fprintf(w, "Re-split chunks         %6d (+%d extra files)\n", r.ResplitChunks, r.ExtraParts)
	fmt.Fprintf(w, "Token re-splits         %6d chunks (+%d extra files, budget %d estimated tokens)\n", r.TokenResplitChunks, r.TokenExtraParts, maxFileTokens)
	fmt.Fprintf(w, "Max file tokens         %6d estimated (limit %d) %s\n", r.MaxFileTokens, maxFileTokens, r.MaxFileTokensKey)
	fmt.Fprintf(w, "Applies-to headers      %6d files\n", r.Headers)
	fmt.Fprintf(w, "Inline <When> notes     %6d\n", r.WhenNotes)
	fmt.Fprintf(w, "Inline <DocCase> notes  %6d\n", r.DocCaseNotes)
	fmt.Fprintf(w, "updated_at fallbacks    %6d files without history\n", r.DateFallbacks)
	fmt.Fprintf(w, "Max sidecar             %6d bytes (limit %d) %s\n", r.MaxSidecarBytes, maxSidecarBytes, r.MaxSidecarKey)

	fmt.Fprintln(w, "\nKB files per product")
	for _, k := range sortedKeys(r.ByProduct) {
		fmt.Fprintf(w, "  %-16s %6d\n", k, r.ByProduct[k])
	}

	if len(r.ByDataset) > 0 {
		fmt.Fprintln(w, "\nGenerated data per dataset (files, bytes of bodies and sidecars)")
		for _, k := range sortedKeys(r.ByDataset) {
			st := r.ByDataset[k]
			fmt.Fprintf(w, "  %-90s %6d files %10d bytes\n", k, st.Files, st.Bytes)
		}
	}

	pageWide, inlineWide := 0, 0
	for _, x := range r.Widened {
		if x.Inline {
			inlineWide++
		} else {
			pageWide++
		}
	}
	fmt.Fprintf(w, "\nWIDENED to [0, %d): %d entries (%d page-level, %d inline)\n", NoUpperBound, len(r.Widened), pageWide, inlineWide)
	for _, x := range r.Widened {
		level := "page"
		if x.Inline {
			level = "inline"
		}
		fmt.Fprintf(w, "  %-6s %s  %s %s\n", level, x.File, x.Component, x.Bound)
	}

	pageUn, inlineUn := 0, 0
	for _, x := range r.Unshipped {
		if x.Inline {
			inlineUn++
		} else {
			pageUn++
		}
	}
	fmt.Fprintf(w, "\nEXCLUDED: no Stack release ships these ranges: %d entries (%d page-level, %d inline); %d chunks and %d inline blocks left out\n",
		len(r.Unshipped), pageUn, inlineUn, r.UnshippedChunks, r.UnshippedBlocks)
	for _, x := range r.Unshipped {
		level := "page"
		if x.Inline {
			level = "inline"
		}
		fmt.Fprintf(w, "  %-6s %s  %s %s\n", level, x.File, x.Component, x.Bound)
	}

	fmt.Fprintf(w, "\nsource_url fallbacks to the site root: %d\n", len(r.URLFallbacks))
	for _, s := range r.URLFallbacks {
		fmt.Fprintf(w, "  %s\n", s)
	}
	if len(r.AnchorMisses) > 0 {
		fmt.Fprintf(w, "\nHeadings without an anchor (URL points at the page): %d\n", len(r.AnchorMisses))
		for _, s := range r.AnchorMisses {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if len(r.Unhandled) > 0 {
		fmt.Fprintf(w, "\nInline <When>/<DocCase> not rewritten: %d\n", len(r.Unhandled))
		for _, s := range r.Unhandled {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if len(r.Empty) > 0 {
		fmt.Fprintf(w, "\nFiles that produced no chunk: %d\n", len(r.Empty))
		for _, s := range r.Empty {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if len(r.ParseFailures) > 0 {
		fmt.Fprintf(w, "\nFiles that failed to parse: %d\n", len(r.ParseFailures))
		for _, s := range r.ParseFailures {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
}

// PrintDiff writes the new, changed and deleted counts and the first keys of
// each; all of them when verbose.
func (d *Diff) PrintDiff(w io.Writer, verbose bool) {
	fmt.Fprintf(w, "\nBucket diff (%d objects under %s)\n", d.Existing, keyPrefix)
	list := func(label string, keys []string) {
		fmt.Fprintf(w, "  %-18s %6d\n", label, len(keys))
		shown := keys
		if !verbose && len(shown) > 20 {
			shown = shown[:20]
		}
		for _, k := range shown {
			fmt.Fprintf(w, "      %s\n", k)
		}
		if len(shown) < len(keys) {
			fmt.Fprintf(w, "      … %d more (--verbose lists all)\n", len(keys)-len(shown))
		}
	}
	list("new", d.New)
	list("changed", d.Changed)
	fmt.Fprintf(w, "  %-18s %6d\n", "unchanged", d.Unchanged)
	list("deleted", d.Deleted)
	list("orphan sidecars", d.OrphanSidecars)
	list("other objects", d.Others)
}
