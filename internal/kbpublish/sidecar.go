package kbpublish

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Limits of the Bedrock metadata contract (support-agent/docs/metadata-contract.md).
const (
	maxSidecarBytes = 900
	maxSidecarKeys  = 35
	maxSourceURLLen = 200
	maxKeyLen       = 1024
	keyPrefix       = "docs/"
	siteURL         = "https://docs.formance.com"
)

// Products a docs file can carry. "webhooks" is new to the contract.
var allowedProducts = []string{"ledger", "payments", "wallets", "flows", "reconciliation", "numscript", "webhooks", "stack", "other"}

// attrKeys are all the attribute names a docs sidecar may carry.
var attrKeys = []string{
	"doc_type", "product", "version", "version_min", "version_below", "version_component",
	"deployment", "license", "client_id", "language", "source_url", "updated_at",
}

// Attrs are the metadataAttributes of one sidecar. Values are strings, except
// version_min and version_below which are JSON numbers (int).
type Attrs map[string]any

var (
	keyCharsRe    = regexp.MustCompile(`^[a-z0-9._/-]+$`)
	updatedAtRe   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	componentIDRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}$`)
)

// Sidecar renders the `<file>.md.metadata.json` content.
func (a Attrs) Sidecar() []byte {
	b, _ := json.Marshal(map[string]any{"metadataAttributes": map[string]any(a)})
	return b
}

// attributesSize is the UTF-8 size of the serialised metadataAttributes, which
// is what the 900-byte limit counts.
func (a Attrs) attributesSize() int {
	b, _ := json.Marshal(map[string]any(a))
	return len(b)
}

// ValidateKey checks an object key against the docs key rules.
func ValidateKey(key string) []string {
	var errs []string
	if !strings.HasPrefix(key, keyPrefix) {
		errs = append(errs, fmt.Sprintf("key %q is not under %s", key, keyPrefix))
	}
	if !strings.HasSuffix(key, ".md") {
		errs = append(errs, fmt.Sprintf("key %q does not end in .md", key))
	}
	if !keyCharsRe.MatchString(key) {
		errs = append(errs, fmt.Sprintf("key %q has characters outside [a-z0-9._/-]", key))
	}
	if len(key) > maxKeyLen {
		errs = append(errs, fmt.Sprintf("key %q is longer than %d bytes", key, maxKeyLen))
	}
	if strings.Contains(key, "//") || slices.Contains(strings.Split(key, "/"), "..") || slices.Contains(strings.Split(key, "/"), ".") {
		errs = append(errs, fmt.Sprintf("key %q has an empty or dot segment", key))
	}
	return errs
}

// Validate checks a sidecar against the metadata contract and returns every
// problem found, not just the first.
func (a Attrs) Validate() []string {
	var errs []string
	addf := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	for k := range a {
		if !slices.Contains(attrKeys, k) {
			addf("unknown attribute %q", k)
		}
	}
	if len(a) > maxSidecarKeys {
		addf("%d attributes, limit is %d", len(a), maxSidecarKeys)
	}
	if n := a.attributesSize(); n > maxSidecarBytes {
		addf("metadataAttributes is %d bytes, limit is %d", n, maxSidecarBytes)
	}

	str := func(k string) (string, bool) {
		v, present := a[k]
		if !present {
			addf("missing attribute %q", k)
			return "", false
		}
		s, ok := v.(string)
		if !ok {
			addf("attribute %q must be a string, got %T", k, v)
			return "", false
		}
		return s, true
	}
	oneOf := func(k string, allowed ...string) {
		if s, ok := str(k); ok && !slices.Contains(allowed, s) {
			addf("attribute %q is %q, allowed: %s", k, s, strings.Join(allowed, ", "))
		}
	}
	num := func(k string) (int, bool) {
		v, present := a[k]
		if !present {
			addf("missing attribute %q", k)
			return 0, false
		}
		switch n := v.(type) {
		case int:
			return n, true
		case int64:
			return int(n), true
		case float64:
			if n == float64(int(n)) {
				return int(n), true
			}
		}
		addf("attribute %q must be an integer number, got %v (%T)", k, v, v)
		return 0, false
	}

	oneOf("doc_type", "doc")
	oneOf("product", allowedProducts...)
	oneOf("version", "any")
	oneOf("deployment", "cloud", "self-hosted", "any")
	oneOf("license", "ee", "oss", "any")
	oneOf("client_id", "none")
	oneOf("language", "en")
	if s, ok := str("version_component"); ok && !componentIDRe.MatchString(s) {
		addf("attribute %q is %q, want [a-z0-9._-]{1,64}", "version_component", s)
	}

	lo, loOK := num("version_min")
	hi, hiOK := num("version_below")
	if loOK && lo < 0 {
		addf("version_min %d is negative", lo)
	}
	if hiOK && hi > NoUpperBound {
		addf("version_below %d is above %d", hi, NoUpperBound)
	}
	if loOK && hiOK && lo >= hi {
		addf("version_min %d is not below version_below %d", lo, hi)
	}

	if u, ok := str("source_url"); ok {
		switch {
		case !strings.HasPrefix(u, "https://"):
			addf("source_url %q is not https", u)
		case len(u) > maxSourceURLLen:
			addf("source_url is %d bytes, limit is %d", len(u), maxSourceURLLen)
		case strings.Contains(u, "?"):
			addf("source_url %q contains '?'", u)
		}
	}
	if d, ok := str("updated_at"); ok {
		if !updatedAtRe.MatchString(d) {
			addf("updated_at %q is not YYYY-MM-DD", d)
		} else if _, err := time.Parse(time.DateOnly, d); err != nil {
			addf("updated_at %q is not a date", d)
		}
	}
	return errs
}
