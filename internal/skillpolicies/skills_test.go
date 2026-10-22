package skillpolicies_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/policy"
)

// request is a skill's `discobox-access request --json` body, as much of it
// as the check reads.
type request struct {
	Purpose     string                         `json:"purpose"`
	Uses        []struct{ Description string } `json:"uses"`
	Statements  []string                       `json:"statements"`
	Definitions map[string]string              `json:"definitions"`
}

// placeholder is what a skill leaves for its reader to fill in: <N>, <slug>,
// <project>.
var placeholder = regexp.MustCompile(`<[A-Za-z][A-Za-z ]*>`)

// requests reads every `discobox-access request --json` heredoc in a file,
// each placeholder filled as a reader would: an issue number for <N>, a word
// for the rest.
func requests(t *testing.T, path string) []request {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	var out []request
	var body *strings.Builder
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		switch {
		case body == nil && strings.Contains(text, "discobox-access request --json <<'EOF'"):
			body = &strings.Builder{}
		case body != nil && strings.TrimSpace(text) == "EOF":
			filled := strings.ReplaceAll(body.String(), "<N>", "312")
			filled = placeholder.ReplaceAllString(filled, "example")
			var req request
			if err := json.Unmarshal([]byte(filled), &req); err != nil {
				t.Fatalf("%s:%d: the request does not read as JSON: %v", path, line, err)
			}
			out = append(out, req)
			body = nil
		case body != nil:
			body.WriteString(text)
			body.WriteString("\n")
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return out
}

// written is a request's policy, refused when it has none or one that does
// not parse and validate, or a sentence not in the form policy.Format writes:
// a lead approves a worker's ask only when its sentences, as the server
// prints them, are its skill's word for word.
func written(t *testing.T, where string, req request) *policy.Policy {
	t.Helper()
	if len(req.Statements) == 0 {
		t.Fatalf("%s: a request asks for a policy, and %q asks for none", where, req.Uses)
	}
	p, err := policy.Written(req.Statements, req.Definitions)
	if err != nil {
		t.Fatalf("%s: %v", where, err)
	}
	for i, sentence := range req.Statements {
		if canonical := policy.Format(p.Statements[i]); canonical != sentence {
			t.Errorf("%s: %q is written %q", where, sentence, canonical)
		}
	}
	return p
}

func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// Every request a skill shows carries a policy that a person could approve.
func TestEverySkillAsksForAPolicy(t *testing.T) {
	dir := root(t)
	paths, err := filepath.Glob(filepath.Join(dir, ".agents", "skills", "*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	paths = append(paths, filepath.Join(dir, "access", "skills", "discobox-access", "SKILL.md"))
	seen := 0
	for _, path := range paths {
		for i, req := range requests(t, path) {
			rel, _ := filepath.Rel(dir, path)
			written(t, fmt.Sprintf("%s request %d", rel, i+1), req)
			seen++
		}
	}
	if seen < 8 {
		t.Fatalf("read %d requests from the skills, want every one they show", seen)
	}
}

// A worker's policy, as deliver-issue and triage-issue ask for it, falls
// within the delegation orchestrate-issues asks for as code compares them:
// every allow within one of the delegation's, every deny carried, every word
// defined alike. Conditions code leaves unsettled are the judge's when the
// lead hands it on, and this does not ask one.
func TestAWorkersPolicyFallsWithinItsLeadsDelegation(t *testing.T) {
	dir := root(t)
	skill := func(name string) string { return filepath.Join(dir, ".agents", "skills", name, "SKILL.md") }
	var delegation *policy.Policy
	for _, req := range requests(t, skill("orchestrate-issues")) {
		if req.Purpose == "delegate" {
			delegation = written(t, "orchestrate-issues delegation", req)
		}
	}
	if delegation == nil {
		t.Fatal("orchestrate-issues asks for no delegation")
	}
	for _, name := range []string{"deliver-issue", "triage-issue"} {
		asked := requests(t, skill(name))
		if len(asked) != 1 {
			t.Fatalf("%s shows %d requests, want its one", name, len(asked))
		}
		given := written(t, name, asked[0])
		if _, err := policy.Within(*given, nil, *delegation, nil); err != nil {
			t.Errorf("%s's policy is not within the lead's delegation: %v", name, err)
		}
	}
}
