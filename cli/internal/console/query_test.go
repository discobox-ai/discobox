package console

import (
	"slices"
	"sort"
	"testing"
)

// A term matches as a subsequence, and lights the tightest window it can at
// its first match; every term must match; a capital makes the query exact.
func TestFuzzyQueryMatchesTheWayFzfDoes(t *testing.T) {
	t.Parallel()
	positions := func(hits map[int]bool) []int {
		var out []int
		for i := range hits {
			out = append(out, i)
		}
		sort.Ints(out)
		return out
	}
	for _, tc := range []struct {
		query, text string
		ok          bool
		lit         []int
	}{
		// The first match, pulled tight: g of GET would start it, but the
		// window ends at github's h and is walked back to github's g.
		{"gith", "GET 200 https://api.github.com", true, []int{20, 21, 22, 23}},
		// First, not best: fzf's first algorithm, since the timeline is not
		// ranked. G of GET and h of https.
		{"gh", "GET 200 https://api.github.com", true, []int{0, 8}},
		{"get gith", "GET 200 https://api.github.com", true, []int{0, 1, 2, 20, 21, 22, 23}},
		{"get gitlab", "GET 200 https://api.github.com", false, nil},
		{"GET", "get 200", false, nil},
		{"GET", "GET 200", true, []int{0, 1, 2}},
		{"post", "POST 201 https://x", true, []int{0, 1, 2, 3}},
		// The window is pulled tight: "ab" in "a_xab" lights the second a.
		{"ab", "a_xab", true, []int{3, 4}},
		{"", "anything", true, nil},
	} {
		hits, ok := ParseQuery(tc.query).Match(tc.text)
		if ok != tc.ok {
			t.Errorf("%q in %q: ok = %v, want %v", tc.query, tc.text, ok, tc.ok)
			continue
		}
		if got := positions(hits); ok && len(got)+len(tc.lit) > 0 && !slices.Equal(got, tc.lit) {
			t.Errorf("%q in %q: lit %v, want %v", tc.query, tc.text, got, tc.lit)
		}
	}
}

// Every term matches inside one field, primary fields rank ahead of detail
// ones, and the whole of a primary field is the best rank there is.
func TestMatchFieldsRanksWithinFields(t *testing.T) {
	tests := []struct {
		query           string
		primary, detail []string
		want            bool
		tier            int
	}{
		{"hcp", []string{"sbx_hcp123"}, nil, true, 2},
		{"hcp", []string{"hcp123"}, nil, true, 1},
		{"hcp", []string{"hcp"}, nil, true, 0},
		{"hcp", []string{"h---c---p"}, nil, true, 3},
		{"hcp", nil, []string{"issue=hcp"}, true, 4},
		{"Repository 717", []string{"box"}, []string{"Repository", "issue=717"}, true, 4},
		{"Repository", nil, []string{"repository"}, false, 0},
		{"ab", []string{"a", "b"}, nil, false, 0},
		{"hcp missing", []string{"hcp"}, nil, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			got, ok := MatchFields(ParseQuery(tt.query), tt.primary, tt.detail)
			if ok != tt.want || ok && got.Tier != tt.tier {
				t.Fatalf("got %+v %v, want tier %d %v", got, ok, tt.tier, tt.want)
			}
		})
	}
}
