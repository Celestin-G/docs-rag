package kbpublish

import (
	"fmt"
	"strings"
	"unicode"
)

// maxFileTokens is the estimated-token budget of one knowledge-base file as
// uploaded (breadcrumb, "Applies to:" header and body together), on top of the
// maxFileBytes cap. Cohere Embed Multilingual v3 accepts 512 tokens of input and
// Bedrock's semantic chunker cannot split inside a code block, so a file that is
// over 512 tokens fails ingestion with "The server encountered an internal
// error". 400 leaves a 20% margin for the estimator being only an estimate.
const maxFileTokens = 400

// Characters per token used by estimateTokens.
const (
	proseCharsPerToken = 4 // English prose
	codeCharsPerToken  = 2 // code, JSON, identifiers, symbol-heavy lines
)

// symbolHeavyRatio is the share of non-space characters of a line that are
// neither letters nor digits above which the line is treated as code.
const symbolHeavyRatio = 0.12

// estimateTokens returns a conservative, deterministic estimate of the number of
// embedding tokens of a knowledge-base file. There is no tokenizer available
// offline, so it is a heuristic calibrated on the two files that failed the
// first real publish: English prose runs at about 4 characters per token, but
// code (and JSON, paths, operators) splits into many short tokens at about 2 to
// 2.5 characters per token. So text inside a code fence, and any line where more
// than 12% of the non-space characters are symbols, counts 2 characters
// per token; other lines count 4. Each line is rounded up and pays for its
// newline, and a character outside ASCII counts as a whole token (multilingual
// tokenizers split those finely). The result errs high, never low.
func estimateTokens(text string) int {
	total := 0
	inFence := false
	for line := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(line)
		fenceLine := strings.HasPrefix(t, "```")
		code := inFence || fenceLine
		if fenceLine {
			inFence = !inFence
		}
		total += lineTokens(line, code)
	}
	return total
}

func lineTokens(line string, code bool) int {
	runes, symbols, nonSpace, wide := 0, 0, 0, 0
	for _, r := range line {
		runes++
		if r > unicode.MaxASCII {
			wide++
		}
		if unicode.IsSpace(r) {
			continue
		}
		nonSpace++
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			symbols++
		}
	}
	per := proseCharsPerToken
	if code || (nonSpace > 0 && float64(symbols) > symbolHeavyRatio*float64(nonSpace)) {
		per = codeCharsPerToken
	}
	n := runes - wide + 1 // the newline
	return (n+per-1)/per + wide
}

// ValidateFileTokens reports a file whose estimated tokens exceed maxFileTokens.
// A file that fails it must never be uploaded.
func ValidateFileTokens(body string) []string {
	if n := estimateTokens(body); n > maxFileTokens {
		return []string{fmt.Sprintf("file is about %d tokens, limit is %d", n, maxFileTokens)}
	}
	return nil
}

// splitForBudgets cuts body into parts such that prefix + parts fit both
// maxFileBytes and maxFileTokens. byteOnly is the number of parts the byte
// budget alone would give, so the caller can tell token-driven splits apart.
// The part budget starts at the byte budget and shrinks by a fifth until every
// file fits; it stops at minTokenPartBudget, where an overflow is left for
// ValidateFileTokens to report.
func splitForBudgets(prefix, body string) (parts []string, byteOnly int) {
	join := func(part string) string {
		if prefix == "" {
			return part
		}
		return prefix + "\n\n" + part
	}
	fits := func(parts []string) bool {
		for _, p := range parts {
			if estimateTokens(join(p)) > maxFileTokens {
				return false
			}
		}
		return true
	}
	budget := max(maxFileBytes-len(prefix)-2, minPartBudget)
	parts = []string{body}
	if len(prefix)+2+len(body) > maxFileBytes {
		parts = splitBody(body, budget)
	}
	byteOnly = len(parts)
	for !fits(parts) && budget > minTokenPartBudget {
		budget = max(budget*4/5, minTokenPartBudget)
		parts = splitBody(body, budget)
	}
	return parts, byteOnly
}

// minTokenPartBudget is the smallest body budget in bytes the token loop tries.
const minTokenPartBudget = 24
