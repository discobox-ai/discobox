package monitors

import (
	"maps"
	"strings"
	"testing"
)

func TestParseOptionsSplitsOnlyWhereAnOptionBegins(t *testing.T) {
	got, err := ParseOptions([]string{"repo=foo/bar,pr=412,413", "watch=prs,actions", "cmd=echo a=b"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"repo": "foo/bar", "pr": "412,413", "watch": "prs,actions", "cmd": "echo a=b"}
	if !maps.Equal(got, want) {
		t.Fatalf("options = %v, want %v", got, want)
	}
}

func TestParseOptionsRefusesAKeyTwiceAndANonOption(t *testing.T) {
	if _, err := ParseOptions([]string{"pr=1", "pr=2"}); err == nil {
		t.Fatal("a key given twice was accepted")
	}
	if _, err := ParseOptions([]string{"justavalue"}); err == nil {
		t.Fatal("a value with no key was accepted")
	}
}

func TestCheckOptionsNamesUnknownAndMissingOptions(t *testing.T) {
	def := Definition{ID: "github-repo", Options: []Option{{Name: "repo", Required: true}, {Name: "pr"}}}
	if err := CheckOptions(def, map[string]string{"repo": "a/b", "pr": "1"}); err != nil {
		t.Fatal(err)
	}
	err := CheckOptions(def, map[string]string{"issues": "1"})
	if err == nil {
		t.Fatal("unknown and missing options were accepted")
	}
	for _, want := range []string{"does not take issues", "it takes repo, pr", "requires repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}
