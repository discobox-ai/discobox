package id

import (
	"strings"
	"testing"
)

func TestNewReturnsPrefixedRandomID(t *testing.T) {
	id, err := New(PrefixSandbox)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(id, PrefixSandbox+"_") {
		t.Fatalf("expected %q prefix, got %q", PrefixSandbox+"_", id)
	}
	random := strings.TrimPrefix(id, PrefixSandbox+"_")
	if len(random) != RandomLength {
		t.Fatalf("expected %d random characters, got %q", RandomLength, id)
	}
	for i := 0; i < len(random); i++ {
		if strings.IndexByte(alphabet, random[i]) < 0 {
			t.Fatalf("unexpected character %q in %q", random[i], id)
		}
	}
	if other := NewString(PrefixSandbox); other == id {
		t.Fatalf("expected unique IDs, got %q twice", id)
	}
}

func TestIsGenerated(t *testing.T) {
	if !IsGenerated(NewString(PrefixSecret)) {
		t.Fatal("expected generated ID to be recognized")
	}
	if !IsGenerated(NewString(PrefixExec)) {
		t.Fatal("expected generated exec ID to be recognized")
	}
	if IsGenerated("user_default") {
		t.Fatal("well-known ID is not generated")
	}
	if IsGenerated("sbx_abc") {
		t.Fatal("partial ID is not generated")
	}
	if IsGenerated("_0123456789abcdef") {
		t.Fatal("empty prefix is not generated")
	}
	if IsGenerated("sbx_ilou456789abcdef") {
		t.Fatal("excluded alphabet characters are not generated")
	}
	if IsGenerated("") {
		t.Fatal("blank value is not generated")
	}
}

func TestRandomPart(t *testing.T) {
	if got, want := RandomPart("sbx_0123456789abcdef"), "0123456789abcdef"; got != want {
		t.Fatalf("RandomPart = %q, want %q", got, want)
	}
	if got, want := RandomPart("noprefix"), "noprefix"; got != want {
		t.Fatalf("RandomPart = %q, want %q", got, want)
	}
}

func TestResolveShort(t *testing.T) {
	candidates := []string{"sbx_dfzx0123456789ab", "sbx_dfzx0123456789cd", "sbx_qqqq0123456789ef", "user_default"}

	for _, tc := range []struct {
		name  string
		short string
		want  []string
	}{
		{name: "exact", short: "sbx_dfzx0123456789ab", want: []string{"sbx_dfzx0123456789ab"}},
		{name: "exact non-generated", short: "user_default", want: []string{"user_default"}},
		{name: "full prefix", short: "sbx_qqqq", want: []string{"sbx_qqqq0123456789ef"}},
		{name: "random part prefix", short: "qqqq", want: []string{"sbx_qqqq0123456789ef"}},
		{name: "ambiguous random part prefix", short: "dfzx", want: []string{"sbx_dfzx0123456789ab", "sbx_dfzx0123456789cd"}},
		{name: "no match", short: "zzzz", want: nil},
		{name: "empty", short: "", want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveShort(tc.short, candidates)
			if len(got) != len(tc.want) {
				t.Fatalf("ResolveShort(%q) = %v, want %v", tc.short, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("ResolveShort(%q) = %v, want %v", tc.short, got, tc.want)
				}
			}
		})
	}
}

func TestResolveShortPrefersFullIDMatch(t *testing.T) {
	// "sbx" is both a full-ID prefix and, hypothetically, a random-part
	// prefix elsewhere; the full-ID match wins and the other is not returned.
	candidates := []string{"sbx_0123456789abcdef", "run_sbx23456789abcdef"}
	got := ResolveShort("sbx", candidates)
	if len(got) != 1 || got[0] != "sbx_0123456789abcdef" {
		t.Fatalf("ResolveShort = %v, want [sbx_0123456789abcdef]", got)
	}
}
