package chunker

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The docs framework generates machine-readable data under
// apps/docs/content/data/generated/. The API reference pages are thin wrappers
// that render these files at build time, so the endpoints, request/response
// schemas and CLI commands exist nowhere else as text — and an ingestion that
// only reads .md/.mdx cannot see any of it.
//
// Two shapes appear. Most datasets are version-composed:
//
//	{"api.operations": {"__component": "stack", "__versions": [
//	   {"range": {"min": "v3.0", "below": "v3.1"}, "data": {"<id>": {...}}}]}}
//
// A few carry no version composition and are a flat map of entries instead
// (api.fctl, for one). Both are handled.

type vccRange struct {
	Min   string `json:"min"`
	Below string `json:"below"`
}

type vccVersion struct {
	Range vccRange                   `json:"range"`
	Data  map[string]json.RawMessage `json:"data"`
}

type vccDataset struct {
	Component string       `json:"__component"`
	Versions  []vccVersion `json:"__versions"`
}

// versionNum turns a framework version label into the compact form used by the
// database: "v3.2" and "3.2" both become 32. Anything else yields 0, meaning
// unbounded — including labels from other components ("fctl-2.0", "sdk-3.1"),
// which are not stack versions and must not be compared against one.
func versionNum(label string) int16 {
	s := strings.TrimPrefix(strings.TrimSpace(label), "v")
	major, minor, ok := strings.Cut(s, ".")
	if !ok {
		return 0
	}
	maj, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	// Only the first component of the minor part: "3.2.1" is not a stack label,
	// but being lenient here costs nothing.
	minor, _, _ = strings.Cut(minor, ".")
	min, err := strconv.Atoi(minor)
	if err != nil {
		return 0
	}
	return int16(maj*10 + min)
}

// ParseGeneratedJSON renders a generated data file into chunks, one per entry
// per distinct version range.
//
// Consecutive ranges whose rendered text is identical are merged: most API
// operations do not change between stack versions, and emitting one chunk per
// range would triple the corpus — and the embedding time with it — for no
// retrieval benefit.
func ParseGeneratedJSON(relPath string, content []byte) []Chunk {
	var file map[string]json.RawMessage
	if err := json.Unmarshal(content, &file); err != nil {
		return nil
	}

	var out []Chunk
	for dataset, raw := range file {
		// Try the version-composed shape first.
		var ds vccDataset
		if err := json.Unmarshal(raw, &ds); err == nil && len(ds.Versions) > 0 {
			out = append(out, chunksFromVersions(relPath, dataset, ds.Versions)...)
			continue
		}
		// Fall back to a flat map of entries, with no version bounds.
		var flat map[string]json.RawMessage
		if err := json.Unmarshal(raw, &flat); err != nil {
			continue
		}
		for _, id := range sortedKeys(flat) {
			if text := renderEntry(dataset, id, flat[id]); text != "" {
				out = append(out, Chunk{
					SourceFile: relPath,
					Section:    sectionLabel(dataset, id),
					Content:    text,
					Deployment: "both",
				})
			}
		}
	}
	return out
}

// chunksFromVersions walks one dataset's version ranges, rendering each entry
// and coalescing consecutive ranges that produce the same text.
func chunksFromVersions(relPath, dataset string, versions []vccVersion) []Chunk {
	// Ranges arrive in ascending order in the generated files; sorting by lower
	// bound makes the coalescing below independent of that assumption.
	sort.SliceStable(versions, func(i, j int) bool {
		return versionNum(versions[i].Range.Min) < versionNum(versions[j].Range.Min)
	})

	// Per entry id, the text seen in each range, in range order.
	type span struct {
		text        string
		from, below int16
	}
	spans := map[string][]span{}
	var ids []string

	for _, v := range versions {
		from, below := versionNum(v.Range.Min), versionNum(v.Range.Below)
		for _, id := range sortedKeys(v.Data) {
			text := renderEntry(dataset, id, v.Data[id])
			if text == "" {
				continue
			}
			prev := spans[id]
			if n := len(prev); n > 0 && prev[n-1].text == text && prev[n-1].below == from {
				// Same content, directly adjacent range: extend rather than repeat.
				prev[n-1].below = below
				spans[id] = prev
				continue
			}
			if len(prev) == 0 {
				ids = append(ids, id)
			}
			spans[id] = append(prev, span{text: text, from: from, below: below})
		}
	}

	sort.Strings(ids)
	var out []Chunk
	for _, id := range ids {
		for _, s := range spans[id] {
			out = append(out, Chunk{
				SourceFile:   relPath,
				Section:      sectionLabel(dataset, id),
				Content:      s.text,
				VersionNum:   s.from,
				VersionBelow: s.below,
				Deployment:   "both",
			})
		}
	}
	return out
}

// renderEntry turns one entry into prose. Datasets with a known shape get a
// tailored rendering; anything else falls back to labelled key/value lines,
// which embeds acceptably and keeps every dataset searchable.
func renderEntry(dataset, id string, raw json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		// Scalars and arrays: render the value under its key.
		var v any
		if json.Unmarshal(raw, &v) != nil {
			return ""
		}
		return strings.TrimSpace(id + ": " + flatten(v))
	}

	switch dataset {
	case "api.operations":
		return renderOperation(id, m)
	case "api.fctl":
		return renderFctl(id, m)
	}
	return renderGeneric(id, m)
}

// renderOperation formats an HTTP endpoint the way someone would search for it:
// method and path first, then the prose, then the response codes.
func renderOperation(id string, m map[string]any) string {
	var b strings.Builder
	method, path := str(m["method"]), str(m["path"])
	if method != "" || path != "" {
		fmt.Fprintf(&b, "%s %s", method, path)
		if s := str(m["summary"]); s != "" {
			b.WriteString(" — " + s)
		}
		b.WriteString("\n\n")
	} else if s := str(m["summary"]); s != "" {
		b.WriteString(s + "\n\n")
	}

	if mod := str(m["module"]); mod != "" {
		b.WriteString("Module: " + mod + "\n")
	}
	b.WriteString("Operation: " + id + "\n")
	if tags := flatten(m["tags"]); tags != "" {
		b.WriteString("Tags: " + tags + "\n")
	}
	if d := str(m["description"]); d != "" {
		b.WriteString("\n" + d + "\n")
	}
	if resp, ok := m["responses"].([]any); ok && len(resp) > 0 {
		b.WriteString("\nResponses:\n")
		for _, r := range resp {
			rm, ok := r.(map[string]any)
			if !ok {
				continue
			}
			fmt.Fprintf(&b, "- %s: %s\n", str(rm["code"]), str(rm["description"]))
		}
	}
	return strings.TrimSpace(b.String())
}

// renderFctl formats a CLI command, leading with the command line itself.
func renderFctl(id string, m map[string]any) string {
	cmd := str(m["command"])
	if cmd == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(cmd + "\n\nfctl command: " + id + "\n")
	if v := str(m["minVersion"]); v != "" {
		b.WriteString("Available from: " + v + "\n")
	}
	return strings.TrimSpace(b.String())
}

// renderGeneric labels each field so the text reads as statements about the
// entry rather than as an opaque object dump.
func renderGeneric(id string, m map[string]any) string {
	var b strings.Builder
	b.WriteString(id + "\n\n")
	for _, k := range sortedAnyKeys(m) {
		if v := flatten(m[k]); v != "" {
			fmt.Fprintf(&b, "%s: %s\n", k, v)
		}
	}
	return strings.TrimSpace(b.String())
}

// flatten renders a JSON value as a single readable line, recursing into
// objects and arrays. Deliberately lossy on deep structures: the goal is
// searchable text, not a faithful serialisation.
func flatten(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			if s := flatten(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		parts := make([]string, 0, len(t))
		for _, k := range sortedAnyKeys(t) {
			if s := flatten(t[k]); s != "" {
				parts = append(parts, k+" "+s)
			}
		}
		return strings.Join(parts, "; ")
	}
	return ""
}

// sectionLabel names the chunk in retrieval logs and in the sources list.
func sectionLabel(dataset, id string) string {
	return dataset + " › " + id
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func sortedKeys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
