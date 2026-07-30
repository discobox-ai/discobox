package cli

import (
	"strings"
	"unicode"
)

// Fuzzy matching scores a subsequence match so that the choices a user most
// likely meant sort first: contiguous runs, matches at word starts, and matches
// near the front of the text all score higher than scattered late ones.
const (
	fuzzyCharScore        = 16
	fuzzyConsecutiveBonus = 15
	fuzzyWordStartBonus   = 10
	fuzzyGapPenalty       = 2
	fuzzyMaxGapPenalty    = 12
	fuzzyLeadingPenalty   = 1
	fuzzyMaxLeading       = 10
)

// fuzzyMatch reports whether every rune of query appears in text in order,
// ignoring case, and returns the score of the best such match along with the
// rune offsets it matched so callers can highlight them. An empty query matches
// with a zero score and no positions.
func fuzzyMatch(text, query string) (score int, positions []int, ok bool) {
	q := []rune(strings.ToLower(query))
	if len(q) == 0 {
		return 0, nil, true
	}
	t := []rune(text)
	if len(q) > len(t) {
		return 0, nil, false
	}
	lower := []rune(strings.ToLower(text))

	// best[j] is the score of the best match of the query prefix handled so far
	// that ends with its final rune at text position j; parent[i][j] records
	// where the previous query rune matched, so the positions can be recovered.
	const none = -1
	best := make([]int, len(t))
	prev := make([]int, len(t))
	parent := make([][]int, len(q))
	for i := range q {
		parent[i] = make([]int, len(t))
	}

	for i, qr := range q {
		copy(prev, best)
		for j := range best {
			best[j] = none
			parent[i][j] = none
		}
		for j, tr := range lower {
			if tr != qr {
				continue
			}
			base := fuzzyCharScore
			if isWordStart(t, j) {
				base += fuzzyWordStartBonus
			}
			if i == 0 {
				best[j] = base - min(j, fuzzyMaxLeading)*fuzzyLeadingPenalty
				continue
			}
			for k := 0; k < j; k++ {
				if prev[k] == none {
					continue
				}
				candidate := prev[k] + base
				if k == j-1 {
					candidate += fuzzyConsecutiveBonus
				} else {
					candidate -= min(j-k-1, fuzzyMaxGapPenalty) * fuzzyGapPenalty
				}
				if best[j] == none || candidate > best[j] {
					best[j] = candidate
					parent[i][j] = k
				}
			}
		}
	}

	end := none
	for j, s := range best {
		if s != none && (end == none || s > best[end]) {
			end = j
		}
	}
	if end == none {
		return 0, nil, false
	}
	positions = make([]int, len(q))
	for i := len(q) - 1; i >= 0; i-- {
		positions[i] = end
		end = parent[i][end]
	}
	return best[positions[len(positions)-1]], positions, true
}

// isWordStart reports whether position j begins a word: the start of the text,
// the first rune after a separator, or a camelCase hump.
func isWordStart(text []rune, j int) bool {
	if j == 0 {
		return true
	}
	prev := text[j-1]
	switch {
	case !unicode.IsLetter(prev) && !unicode.IsDigit(prev):
		return true
	case unicode.IsLower(prev) && unicode.IsUpper(text[j]):
		return true
	}
	return false
}
