package policy

import (
	"strings"
	"testing"
)

// A written policy is parsed and validated before anything is sent: no
// statements is no policy, definitions alone are refused, and what is wrong
// says how a statement reads.
func TestWritten(t *testing.T) {
	if p, err := Written(nil, nil); p != nil || err != nil {
		t.Fatalf("Written(nothing) = %+v, %v, want no policy", p, err)
	}
	if _, err := Written(nil, map[string]string{"push": "x"}); err == nil || !strings.Contains(err.Error(), "no statements") {
		t.Fatalf("definitions alone = %v, want them refused", err)
	}
	if _, err := Written([]string{"allow read issue"}, nil); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("a sentence that does not parse = %v", err)
	}
	if _, err := Written([]string{`allow push on branch "x" in repository $primary.repo`}, nil); err == nil || !strings.Contains(err.Error(), Grammar) {
		t.Fatalf("a word with no definition = %v, want it refused with the grammar", err)
	}
	p, err := Written([]string{"deny delete on anything in anywhere"}, nil)
	if err != nil || len(p.Statements) != 1 {
		t.Fatalf("a valid policy = %+v, %v", p, err)
	}
}

// --define takes one WORD=DEFINITION at a time, and a word once.
func TestDefinitions(t *testing.T) {
	d := Definitions{}
	if err := d.Set("push = update a branch's commits"); err != nil || d["push"] != " update a branch's commits" {
		t.Fatalf("Set() = %v, %q", err, d["push"])
	}
	for _, bad := range []string{"push=again", "push", "=x"} {
		if err := d.Set(bad); err == nil {
			t.Errorf("Set(%q) was accepted", bad)
		}
	}
}
