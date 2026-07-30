package cli

import "testing"

func TestFuzzyMatchSubsequences(t *testing.T) {
	for _, tc := range []struct {
		text, query string
		want        bool
	}{
		{"api-server", "apis", true},
		{"api-server", "asv", true},
		{"api-server", "API", true},
		{"api-server", "xyz", false},
		{"api-server", "srevre", false},
		{"api", "apis", false},
		{"anything", "", true},
	} {
		if _, _, ok := fuzzyMatch(tc.text, tc.query); ok != tc.want {
			t.Errorf("fuzzyMatch(%q, %q) ok = %v, want %v", tc.text, tc.query, ok, tc.want)
		}
	}
}

func TestFuzzyMatchPositionsCoverTheQuery(t *testing.T) {
	_, positions, ok := fuzzyMatch("api-server", "asv")
	if !ok {
		t.Fatal("fuzzyMatch did not match")
	}
	if len(positions) != 3 {
		t.Fatalf("positions = %v, want one per query rune", positions)
	}
	runes := []rune("api-server")
	want := []rune{'a', 's', 'v'}
	for i, pos := range positions {
		if runes[pos] != want[i] {
			t.Fatalf("positions[%d] = %d (%q), want a %q", i, pos, runes[pos], want[i])
		}
		if i > 0 && pos <= positions[i-1] {
			t.Fatalf("positions %v are not increasing", positions)
		}
	}
}

// Contiguous, word-start, and early matches are what a user means by "closer",
// so they must score above scattered or late ones.
func TestFuzzyMatchPrefersContiguousAndEarlyMatches(t *testing.T) {
	contiguous, _, _ := fuzzyMatch("apiary", "api")
	scattered, _, _ := fuzzyMatch("a-p-i-x", "api")
	if contiguous <= scattered {
		t.Errorf("contiguous score %d, scattered score %d, want contiguous higher", contiguous, scattered)
	}

	early, _, _ := fuzzyMatch("server-logs", "ser")
	late, _, _ := fuzzyMatch("logs-for-the-server", "ser")
	if early <= late {
		t.Errorf("early score %d, late score %d, want early higher", early, late)
	}

	wordStart, _, _ := fuzzyMatch("api-server", "s")
	midWord, _, _ := fuzzyMatch("assistant", "s")
	if wordStart <= midWord {
		t.Errorf("word-start score %d, mid-word score %d, want word start higher", wordStart, midWord)
	}
}

func TestFuzzyPickerMatchesRankTitleOverDetail(t *testing.T) {
	items := []pickerItem{
		{id: "sbx_1", title: "docs", detail: "running · now"},
		{id: "sbx_2", title: "run-tests", detail: "stopped · now"},
	}
	matches := fuzzyPickerMatches(items, "run", "")
	if len(matches) != 2 {
		t.Fatalf("matches = %d, want both items", len(matches))
	}
	if matches[0].item.id != "sbx_2" {
		t.Fatalf("top match = %q, want the title match sbx_2", matches[0].item.id)
	}
	if len(matches[0].titlePos) != 3 {
		t.Fatalf("titlePos = %v, want a position per query rune", matches[0].titlePos)
	}
}

func TestFuzzyPickerMatchesEmptyQueryKeepsOrder(t *testing.T) {
	items := []pickerItem{{id: "sbx_1"}, {id: "sbx_2"}, {id: "sbx_3"}}
	matches := fuzzyPickerMatches(items, "", "")
	if len(matches) != 3 {
		t.Fatalf("matches = %d, want 3", len(matches))
	}
	for i, match := range matches {
		if match.index != i || match.item.id != items[i].id {
			t.Fatalf("matches[%d] = %+v, want the list order preserved", i, match)
		}
	}
}

func TestFuzzyPickerMatchesFindIDs(t *testing.T) {
	items := []pickerItem{{id: "sbx_7hq2", title: "docs"}, {id: "sbx_a1b2", title: "api"}}
	matches := fuzzyPickerMatches(items, "7hq", "")
	if len(matches) != 1 || matches[0].item.id != "sbx_7hq2" {
		t.Fatalf("matches = %+v, want only sbx_7hq2", matches)
	}
	if len(matches[0].idPos) != 3 {
		t.Fatalf("idPos = %v, want a position per query rune", matches[0].idPos)
	}
}
