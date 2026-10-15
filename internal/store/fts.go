package store

import (
	"strings"
	"unicode"
)

// MatchMode selects how token predicates are combined.
type MatchMode int

const (
	// MatchAll requires every token (AND). Used by keyword search.
	MatchAll MatchMode = iota
	// MatchAny keeps a memory if any token matches (OR). Used by recall.
	MatchAny
)

const prefixMinRunes = 3

// stopwords are dropped from natural-language queries when any other token remains.
// They are not stemmed and they are not a ranking signal.
var stopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "and": {}, "or": {}, "if": {},
	"in": {}, "on": {}, "of": {}, "to": {}, "for": {}, "with": {},
	"by": {}, "at": {}, "from": {}, "as": {}, "is": {}, "it": {},
	"be": {}, "we": {}, "our": {}, "this": {}, "that": {},
}

// Tokens splits a query into lowercase words the same way a unicode61 FTS5
// index does: letters and digits stay, everything else separates words.
// Tokens shorter than two characters are dropped. Stopwords are dropped when
// the query still has another word.
func Tokens(query string) []string {
	raw := scanTokens(query)
	kept := make([]string, 0, len(raw))
	for _, tok := range raw {
		if _, skip := stopwords[tok]; skip {
			continue
		}
		kept = append(kept, tok)
	}
	if len(kept) == 0 {
		return raw
	}
	return kept
}

func scanTokens(query string) []string {
	var b strings.Builder
	var out []string
	flush := func() {
		if b.Len() == 0 {
			return
		}
		tok := b.String()
		b.Reset()
		if len([]rune(tok)) < 2 {
			return
		}
		out = append(out, tok)
	}
	for _, r := range strings.ToLower(query) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			continue
		}
		flush()
	}
	flush()
	return out
}

// BuildMatch returns an FTS5 MATCH expression made only of quoted tokens,
// or an empty string when the query has no usable terms.
// Tokens of at least three characters are prefix queries so "auth" matches
// "authentication". The result is safe to bind as a single SQL parameter.
func BuildMatch(query string, mode MatchMode) string {
	toks := Tokens(query)
	if len(toks) == 0 {
		return ""
	}
	parts := make([]string, len(toks))
	for i, tok := range toks {
		parts[i] = quoteToken(tok, true)
	}
	op := " AND "
	if mode == MatchAny {
		op = " OR "
	}
	return strings.Join(parts, op)
}

func quoteToken(tok string, allowPrefix bool) string {
	quoted := `"` + strings.ReplaceAll(tok, `"`, `""`) + `"`
	if allowPrefix && len([]rune(tok)) >= prefixMinRunes {
		return quoted + "*"
	}
	return quoted
}
