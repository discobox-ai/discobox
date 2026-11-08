package console

import (
	"cmp"
	"strings"
	"unicode"
)

// Query is a filter read the way fzf reads one: each word of it is a term
// that must appear in the text in order but not necessarily together —
// "gh200" finds "GET 200 https://api.github.com" — and every term must appear.
// It is smart-case: lower case matches either case, and a query with a capital
// in it matches case exactly.
type Query struct {
	terms         [][]rune
	caseSensitive bool
}

// ParseQuery reads a query as typed.
func ParseQuery(query string) Query {
	q := Query{caseSensitive: strings.IndexFunc(query, unicode.IsUpper) >= 0}
	for _, term := range strings.Fields(query) {
		q.terms = append(q.terms, []rune(term))
	}
	return q
}

// Empty reports whether the query has no terms, which matches everything.
func (q Query) Empty() bool { return len(q.terms) == 0 }

// Match reports whether text holds every term, and which of its runes matched
// them, by rune index. An empty query matches everything and marks nothing.
func (q Query) Match(text string) (hits map[int]bool, ok bool) {
	if q.Empty() {
		return nil, true
	}
	runes := []rune(text)
	hits = map[int]bool{}
	for _, term := range q.terms {
		if !q.matchTerm(runes, term, hits) {
			return nil, false
		}
	}
	return hits, true
}

// matchTerm finds one term in text, marking where. It is fzf's first
// algorithm: the earliest place the term can end, then back from there to the
// latest place it can start — the tightest window at the first match, so what
// is lit reads as the thing that was found rather than letters strewn along
// the line.
func (q Query) matchTerm(text, term []rune, hits map[int]bool) bool {
	end, t := -1, 0
	for i, r := range text {
		if q.same(r, term[t]) {
			t++
			if t == len(term) {
				end = i
				break
			}
		}
	}
	if end < 0 {
		return false
	}
	t = len(term) - 1
	for i := end; i >= 0 && t >= 0; i-- {
		if q.same(text[i], term[t]) {
			hits[i] = true
			t--
		}
	}
	return true
}

func (q Query) same(a, b rune) bool {
	if q.caseSensitive {
		return a == b
	}
	return unicode.ToLower(a) == unicode.ToLower(b)
}

// Rank is how well a candidate matched a query (MatchFields), lower first:
// the tier of its worst term — 0 an exact field, 1 a field's prefix, 2 a run
// inside one, 3 a subsequence, 4 anything in a detail field — then the cells
// its terms left between their runes, then how far into their fields they
// started. It carries the evidence the palette ranks by without changing the
// acceptance rules of the filters that also use Query.
type Rank struct{ Tier, Gaps, Start int }

// Compare orders two ranks, the better first.
func (a Rank) Compare(b Rank) int {
	if n := cmp.Compare(a.Tier, b.Tier); n != 0 {
		return n
	}
	if n := cmp.Compare(a.Gaps, b.Gaps); n != 0 {
		return n
	}
	return cmp.Compare(a.Start, b.Start)
}

// MatchFields matches a query against a candidate's fields (ADR 26-11-06-671
// §5): every term must match inside one field — terms may match different
// fields, but no term crosses from one field into the next — and primary
// fields rank ahead of detail ones. A query that is the whole of a primary
// field is the best rank there is.
func MatchFields(q Query, primary, detail []string) (Rank, bool) {
	rank := Rank{}
	for _, term := range q.terms {
		best := Rank{Tier: 99}
		for group, fields := range [][]string{primary, detail} {
			for _, field := range fields {
				hits := map[int]bool{}
				rr := []rune(field)
				if !q.matchTerm(rr, term, hits) {
					continue
				}
				lo, hi := len(rr), 0
				for i := range hits {
					lo = min(lo, i)
					hi = max(hi, i)
				}
				r := Rank{Tier: 3, Gaps: hi - lo + 1 - len(term), Start: lo}
				if r.Gaps == 0 {
					r.Tier = 2
					if lo == 0 {
						r.Tier = 1
						if len(rr) == len(term) {
							r.Tier = 0
						}
					}
				}
				if group == 1 {
					r.Tier = 4
				}
				if r.Compare(best) < 0 {
					best = r
				}
			}
		}
		if best.Tier == 99 {
			return Rank{}, false
		}
		rank.Tier = max(rank.Tier, best.Tier)
		rank.Gaps += best.Gaps
		rank.Start += best.Start
	}
	if !q.Empty() {
		var terms []string
		for _, t := range q.terms {
			terms = append(terms, string(t))
		}
		query := strings.Join(terms, " ")
		for _, field := range primary {
			same := field == query
			if !q.caseSensitive {
				same = strings.EqualFold(field, query)
			}
			if same {
				return Rank{}, true
			}
		}
	}
	return rank, true
}
