package kbpublish

import (
	"fmt"
	"strings"
)

// Conversion is a component version range converted to the Stack scale.
type Conversion struct {
	Component        string
	Min, Below       int // Stack scale; 0 and NoUpperBound when unbounded or widened
	MinRaw, BelowRaw string

	// A bound that is present but cannot be expressed on the Stack scale is
	// widened: the chunk is shown to every Stack version rather than hidden from
	// the ones it may apply to. WidenedMin/WidenedBelow record which.
	WidenedMin, WidenedBelow bool
	// Empty marks a range that collapsed (min >= below) once converted: no Stack
	// release ships it. Both bounds are widened in that case.
	Empty bool
	// MinTrivial marks a lower bound at or below the first Stack release: it
	// restricts nothing, so no phrase names it. Min keeps its value.
	MinTrivial bool
}

// Widened reports whether any bound was widened.
func (c Conversion) Widened() bool { return c.WidenedMin || c.WidenedBelow }

// Convert converts a component's raw bounds ("" for none) to the Stack scale.
func (t *StackTable) Convert(component, minRaw, belowRaw string) Conversion {
	if component == "" {
		component = "stack"
	}
	c := Conversion{Component: component, Below: NoUpperBound, MinRaw: minRaw, BelowRaw: belowRaw}
	if minRaw != "" {
		if v, ok := t.Lower(component, minRaw); ok {
			c.Min = v
		} else {
			c.WidenedMin = true
		}
	}
	if belowRaw != "" {
		if v, ok := t.Upper(component, belowRaw); ok {
			c.Below = v
		} else {
			c.WidenedBelow = true
		}
	}
	if c.Min > 0 && !c.WidenedMin && c.Min <= t.firstStack() {
		c.MinTrivial = true
	}
	if !c.Widened() && c.Min >= c.Below {
		c.Empty = true
		c.Min, c.Below = 0, NoUpperBound
		c.WidenedMin, c.WidenedBelow = c.MinRaw != "", c.BelowRaw != ""
	}
	return c
}

// stackLabel renders a Stack-scale number: 302 -> "v3.2".
func stackLabel(scale int) string { return fmt.Sprintf("v%d.%d", scale/100, scale%100) }

// fromPhrase and beforePhrase name one bound, on the Stack scale when it was
// converted and on the component's own scale otherwise.
func (c Conversion) fromPhrase() string {
	switch {
	case c.MinRaw == "":
		return ""
	case c.WidenedMin:
		return c.Component + " " + c.MinRaw
	case c.Min == 0 || c.MinTrivial:
		return ""
	}
	return "Stack " + stackLabel(c.Min)
}

func (c Conversion) beforePhrase() string {
	switch {
	case c.BelowRaw == "":
		return ""
	case c.WidenedBelow:
		return c.Component + " " + c.BelowRaw
	case c.Below == NoUpperBound:
		return ""
	}
	return "Stack " + stackLabel(c.Below)
}

// shortBefore drops from the "before" phrase the prefix it shares with the
// "from" phrase ("Stack " or the component name), so ranges read "Stack v3.1 up
// to before v3.2" and "membership 2.0.0 up to before 2.3.0".
func (c Conversion) shortBefore(from, before string) string {
	for _, p := range []string{"Stack ", c.Component + " "} {
		if strings.HasPrefix(from, p) && strings.HasPrefix(before, p) {
			return strings.TrimPrefix(before, p)
		}
	}
	return before
}

// inlinePhrase is the version part of an inline note: "from Stack v3.1, before
// v3.2". Empty when the range carries no constraint.
func (c Conversion) inlinePhrase() string {
	from, before := c.fromPhrase(), c.beforePhrase()
	var parts []string
	if from != "" {
		parts = append(parts, "from "+from)
	}
	if before != "" {
		before = c.shortBefore(from, before)
		parts = append(parts, "before "+before)
	}
	return strings.Join(parts, ", ")
}

// headerPhrase is the version part of the "Applies to:" header.
func (c Conversion) headerPhrase() string {
	from, before := c.fromPhrase(), c.beforePhrase()
	var s string
	switch {
	case from != "" && before != "":
		before = c.shortBefore(from, before)
		s = from + " up to before " + before
	case from != "":
		s = from + " and later"
	case before != "":
		s = "before " + before
	default:
		return ""
	}
	// Name the original bound when the page is scoped to another component, so
	// the reader sees both scales. Only the bounds the phrase shows are named.
	if c.Component != "stack" && !c.Widened() {
		var orig string
		switch {
		case from != "" && before != "":
			orig = c.MinRaw + " up to before " + c.BelowRaw
		case from != "":
			orig = c.MinRaw + " and later"
		default:
			orig = "before " + c.BelowRaw
		}
		s += " (" + c.Component + " " + orig + ")"
	}
	return s
}

// Audience is the non-version gating of a chunk.
type Audience struct {
	License    string // "ee", "oss" or ""
	Deployment string // "cloud", "self-hosted" or ""
	Numscript  string // "new", "legacy" or ""
	Experiment bool
}

func (a Audience) phrases() []string {
	var out []string
	switch a.License {
	case "ee":
		out = append(out, "Enterprise Edition")
	case "oss":
		out = append(out, "open source edition")
	}
	switch a.Deployment {
	case "cloud":
		out = append(out, "Cloud")
	case "self-hosted":
		out = append(out, "self-hosted")
	}
	switch a.Numscript {
	case "new":
		out = append(out, "new Numscript interpreter")
	case "legacy":
		out = append(out, "legacy Numscript machine")
	}
	return out
}

// InlineNote is the parenthesised note put in front of a <When> block:
// "(Applies from Stack v3.1, before v3.2, to Enterprise Edition)". Empty when the
// block constrains nothing.
func InlineNote(c Conversion, a Audience) string {
	var parts []string
	if p := c.inlinePhrase(); p != "" {
		parts = append(parts, p)
	}
	if aud := a.phrases(); len(aud) > 0 {
		parts = append(parts, "to "+strings.Join(aud, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(Applies " + strings.Join(parts, ", ") + ")"
}

// Header is the "Applies to:" line of a gated page or section. Empty when the
// content applies everywhere.
func Header(c Conversion, a Audience) string {
	var items []string
	if p := c.headerPhrase(); p != "" {
		items = append(items, p)
	}
	items = append(items, a.phrases()...)
	if a.Experiment {
		items = append(items, "experimental feature")
	}
	if len(items) == 0 {
		return ""
	}
	return "Applies to: " + strings.Join(items, "; ")
}
