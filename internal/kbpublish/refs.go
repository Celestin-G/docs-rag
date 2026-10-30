package kbpublish

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/celestingeniaut/docs-rag/internal/docsource"
)

// A fragment or a generated dataset has no URL of its own: the reader sees it
// inside the page that includes it. refIndex finds that page by reading the
// MDX: <Fragment id="…"/> pulls a fragment into a page, and <DocDataTable
// k="dataset"/> (or a {{dataset}} placeholder) pulls a dataset into a page or a
// fragment.

var (
	fragmentRefRe = regexp.MustCompile(`<Fragment\s+id=["']([^"']+)["']`)
	dataTableRe   = regexp.MustCompile(`(?s)<DocDataTable\b[^>]*?\bk=["']([^"']+)["']`)
	placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z0-9._-]+)\s*\}\}`)
	fmPathRe      = regexp.MustCompile(`(?m)^path:\s*(.+)$`)
	fmIDRe        = regexp.MustCompile(`(?m)^id:\s*(.+)$`)
)

type refIndex struct {
	pagePaths map[string]bool            // frontmatter paths of pages, for sanity
	kPages    map[string]map[string]bool // dataset key -> page paths
	fragPages map[string]map[string]bool // fragment id -> page paths
	fragID    map[string]string          // fragment file (rel) -> id
}

// frontmatterOf returns the frontmatter block of an MDX file, "" if none.
func frontmatterOf(content string) string {
	rest, ok := strings.CutPrefix(content, "---")
	if !ok {
		return ""
	}
	fm, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return ""
	}
	return fm
}

func fmValue(re *regexp.Regexp, fm string) string {
	if m := re.FindStringSubmatch(fm); m != nil {
		return strings.Trim(strings.TrimSpace(m[1]), `"'`)
	}
	return ""
}

func add(m map[string]map[string]bool, k, v string) bool {
	if m[k] == nil {
		m[k] = map[string]bool{}
	}
	if m[k][v] {
		return false
	}
	m[k][v] = true
	return true
}

// fragmentIDFromPath is the id a fragment gets when its frontmatter has none.
func fragmentIDFromPath(rel string) string {
	p := strings.TrimPrefix(rel, "apps/docs/content/fragments/")
	return strings.TrimSuffix(p, path.Ext(p))
}

func buildRefs(sources []SourceFile) *refIndex {
	idx := &refIndex{
		pagePaths: map[string]bool{},
		kPages:    map[string]map[string]bool{},
		fragPages: map[string]map[string]bool{},
		fragID:    map[string]string{},
	}
	type frag struct {
		id     string
		nested []string
		ks     []string
	}
	var frags []frag
	for _, s := range sources {
		kind := docsource.Kind(s.Rel)
		if kind != docsource.KindPage && kind != docsource.KindFragment {
			continue
		}
		content := string(s.Content)
		fm := frontmatterOf(content)
		var nested, ks []string
		for _, m := range fragmentRefRe.FindAllStringSubmatch(content, -1) {
			nested = append(nested, m[1])
		}
		for _, m := range dataTableRe.FindAllStringSubmatch(content, -1) {
			ks = append(ks, m[1])
		}
		for _, m := range placeholderRe.FindAllStringSubmatch(content, -1) {
			ks = append(ks, m[1])
		}
		if kind == docsource.KindPage {
			p := fmValue(fmPathRe, fm)
			if p == "" {
				continue
			}
			idx.pagePaths[p] = true
			for _, id := range nested {
				add(idx.fragPages, id, p)
			}
			for _, k := range ks {
				add(idx.kPages, k, p)
			}
			continue
		}
		id := fmValue(fmIDRe, fm)
		if id == "" {
			id = fragmentIDFromPath(s.Rel)
		}
		idx.fragID[s.Rel] = id
		frags = append(frags, frag{id: id, nested: nested, ks: ks})
	}
	// A fragment included by a fragment reaches the pages of its includer. Pages
	// nest shallowly; a few passes reach the fixpoint.
	for range 6 {
		changed := false
		for _, f := range frags {
			for _, n := range f.nested {
				for p := range idx.fragPages[f.id] {
					if add(idx.fragPages, n, p) {
						changed = true
					}
				}
			}
		}
		if !changed {
			break
		}
	}
	for _, f := range frags {
		for _, k := range f.ks {
			for p := range idx.fragPages[f.id] {
				add(idx.kPages, k, p)
			}
		}
	}
	return idx
}

func firstOf(set map[string]bool) string {
	if len(set) == 0 {
		return ""
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// pageOfFragment is the first page (by path) that includes the fragment file.
func (r *refIndex) pageOfFragment(rel string) string {
	return firstOf(r.fragPages[r.fragID[rel]])
}

// pageOfDataset finds the page showing an entry of a dataset: a table keyed on
// "dataset.id…" is more specific than one keyed on the dataset alone.
func (r *refIndex) pageOfDataset(dataset, id string) string {
	pages := map[string]bool{}
	collect := func(match func(k string) bool) {
		for k, ps := range r.kPages {
			if match(k) {
				for p := range ps {
					pages[p] = true
				}
			}
		}
	}
	if id != "" && id != dataset {
		entry := dataset + "." + id
		collect(func(k string) bool { return k == entry || strings.HasPrefix(k, entry+".") })
	}
	if len(pages) == 0 {
		collect(func(k string) bool { return k == dataset || strings.HasPrefix(k, dataset+".") })
	}
	return firstOf(pages)
}
