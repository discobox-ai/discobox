package access

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// The sentences the help teaches, with the definitions their words need.
var (
	helpStatements = []string{
		`allow read on anything in repository $primary.repo`,
		`allow push on branch "fix-43" in repository $primary.repo`,
		`allow create, update on pull request in repository $primary.repo when "its base is main"`,
		`deny delete on anything in anywhere`,
	}
	helpDefinitions = map[string]string{
		"repository":   "a GitHub repository",
		"branch":       "a git branch",
		"pull request": "a GitHub pull request",
		"push":         "update a branch's commits",
	}
)

func helpPolicy(t *testing.T) *policy.Policy {
	t.Helper()
	statements, err := policy.ParseStatements(helpStatements)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return &policy.Policy{Statements: statements, Definitions: helpDefinitions}
}

// An agent writes sentences, from either form of the command, and the
// service is sent the structure they parse to.
func TestRequestSendsThePolicyItsStatementsSay(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	want := helpPolicy(t)

	args := []string{"request", "com.github.api", "--use", "Fix issue #43 in acme/web as a pull request"}
	for _, s := range helpStatements {
		args = append(args, "--statement", s)
	}
	for word, definition := range helpDefinitions {
		args = append(args, "--define", word+"="+definition)
	}
	if _, stderr, code := capture(t, "", func() int { return Run(args) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if !reflect.DeepEqual(svc.gotRequest.Policy, want) {
		t.Fatalf("policy = %#v, want %#v", svc.gotRequest.Policy, want)
	}

	svc.gotRequest = agentcreds.RequestBody{}
	body, err := json.Marshal(requestInput{
		ID:          "com.github.api",
		Uses:        []agentcreds.RequestedUse{{Description: "Fix issue #43 in acme/web as a pull request"}},
		Statements:  helpStatements,
		Definitions: helpDefinitions,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, stderr, code := capture(t, string(body), func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if !reflect.DeepEqual(svc.gotRequest.Policy, want) {
		t.Fatalf("policy = %#v, want %#v from the JSON body", svc.gotRequest.Policy, want)
	}

	// Uses alone are a request as before, with no policy.
	svc.gotRequest = agentcreds.RequestBody{}
	if _, stderr, code := capture(t, "", func() int { return Run([]string{"request", "com.github.api", "--use", "Open a PR"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if svc.gotRequest.Policy != nil {
		t.Fatalf("policy = %#v, want none for a request of uses alone", svc.gotRequest.Policy)
	}
}

// What an agent wrote wrong is refused before anything is sent, with what
// to fix and what a statement reads as, so it can write it again.
func TestRequestRefusesAPolicyNobodyCouldApproveLocally(t *testing.T) {
	cases := map[string]struct {
		args []string
		want []string
	}{
		"a sentence that does not parse": {
			args: []string{"--statement", "deny wat"},
			want: []string{"statement 1", `"on"`, policy.Grammar},
		},
		"a word with no definition": {
			args: []string{"--statement", `allow push on branch "fix-43" in repository $primary.repo`, "--define", "branch=a git branch", "--define", "repository=a GitHub repository"},
			want: []string{`"push" is not a default word and has no definition`, policy.Grammar},
		},
		"an allow that applies anywhere": {
			args: []string{"--statement", "allow read on anything in anywhere"},
			want: []string{"only a deny may apply anywhere"},
		},
		"definitions with no statements": {
			args: []string{"--define", "push=update a branch's commits"},
			want: []string{"has no statements"},
		},
		"a definition that is not WORD=DEFINITION": {
			args: []string{"--statement", "deny delete on anything in anywhere", "--define", "push"},
			want: []string{"WORD=DEFINITION"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			svc := &fakeService{}
			serve(t, svc)
			args := append([]string{"request", "com.github.api", "--use", "Fix issue #43"}, tc.args...)
			_, stderr, code := capture(t, "", func() int { return Run(args) })
			if code != exitUsage {
				t.Fatalf("exit = %d, want %d: %s", code, exitUsage, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr = %s; want it to say %q", stderr, want)
				}
			}
			if svc.gotRequest.ID != "" {
				t.Fatalf("request = %#v, want nothing sent", svc.gotRequest)
			}
		})
	}

	// A JSON body is held to the same, and the failure is structured.
	svc := &fakeService{}
	serve(t, svc)
	body := `{"id":"com.github.api","uses":[{"description":"Fix issue #43"}],"statements":["allow read on anything in repository acme/web"]}`
	_, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	var failure errorEnvelope
	if err := json.Unmarshal([]byte(stderr), &failure); err != nil || code != exitUsage {
		t.Fatalf("exit = %d, stderr = %s; want a structured usage error", code, stderr)
	}
	if failure.Error.Code != agentcreds.CodeInvalid || !strings.Contains(failure.Error.Message, "a name is quoted or a variable") {
		t.Fatalf("failure = %#v, want code invalid saying how to write the name", failure)
	}
	if svc.gotRequest.ID != "" {
		t.Fatalf("request = %#v, want nothing sent", svc.gotRequest)
	}
}

// A granted policy reads back as the sentences an agent writes, with each
// word's definition and what each variable resolved to; --json carries the
// structure and the sentences both.
func TestAGrantedPolicyIsShownAsSentences(t *testing.T) {
	granted := helpPolicy(t)
	granted.Statements = append(granted.Statements, policy.Statement{
		Effect: policy.Allow, Actions: []string{policy.ActionRead}, ResourceType: policy.Anything,
		Scope: policy.Scope{Type: "repository", Name: policy.VarPrimaryUpstream},
	})
	granted.Resolved = map[string]string{policy.VarPrimaryRepo: "acme/web"}
	svc := &fakeService{status: agentcreds.RequestStatus{
		RequestID: "sreq_1",
		Status:    agentcreds.StatusGranted,
		Purpose:   agentcreds.PurposeUse,
		Uses:      []agentcreds.Use{{UseID: "use_1", Description: "Fix issue #43 in acme/web as a pull request"}},
		Policy:    granted,
	}}
	serve(t, svc)

	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"wait", "request", "sreq_1"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	for _, want := range append(helpStatements,
		`allow read on anything in repository $primary.upstream`,
		`"push" means: update a branch's commits`,
		`$primary.repo is "acme/web"`,
		`$primary.upstream resolved to nothing, so it matches nothing`,
	) {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %s; want it to show %q", stdout, want)
		}
	}

	stdout, stderr, code = capture(t, "", func() int { return Run([]string{"wait", "--json", "request", "sreq_1"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	var decoded struct {
		agentcreds.RequestStatus
		Policy struct {
			policy.Policy
			Sentences []string `json:"sentences"`
		} `json:"policy"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if !reflect.DeepEqual(&decoded.Policy.Policy, granted) {
		t.Fatalf("policy = %#v, want the structure granted", decoded.Policy.Policy)
	}
	if want := policy.FormatStatements(granted.Statements); !reflect.DeepEqual(decoded.Policy.Sentences, want) {
		t.Fatalf("sentences = %q, want %q", decoded.Policy.Sentences, want)
	}
}

// A policy nobody has approved yet is shown without a claim about what its
// variables resolved to: they resolve at approval.
func TestAnUnapprovedPolicyMakesNoClaimAboutItsVariables(t *testing.T) {
	var b strings.Builder
	writePolicy(&b, helpPolicy(t), false)
	if !strings.Contains(b.String(), helpStatements[0]) || strings.Contains(b.String(), "resolved to nothing") {
		t.Fatalf("shown = %s; want the statements and no claim about variables", b.String())
	}
}

// list shows each credential's policy as sentences, and --json carries it
// with its sentences.
func TestListShowsAPolicyAsSentences(t *testing.T) {
	granted := helpPolicy(t)
	granted.Resolved = map[string]string{policy.VarPrimaryRepo: "acme/web"}
	serve(t, &fakeService{credentials: []agentcreds.Credential{{
		Name:   "github",
		EnvVar: "GH_TOKEN",
		Hosts:  []string{"github.com"},
		Uses:   []agentcreds.Use{{UseID: "use_1", Description: "Fix issue #43 in acme/web as a pull request"}},
		Policy: granted,
	}}})

	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	for _, want := range []string{helpStatements[1], `$primary.repo is "acme/web"`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %s; want it to show %q", stdout, want)
		}
	}

	stdout, _, _ = capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	var decoded struct {
		Credentials []struct {
			Policy struct {
				Statements []policy.Statement `json:"statements"`
				Sentences  []string           `json:"sentences"`
			} `json:"policy"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil || len(decoded.Credentials) != 1 {
		t.Fatalf("stdout = %s (%v); want one credential", stdout, err)
	}
	got := decoded.Credentials[0].Policy
	if !reflect.DeepEqual(got.Statements, granted.Statements) || !reflect.DeepEqual(got.Sentences, policy.FormatStatements(granted.Statements)) {
		t.Fatalf("policy = %#v, want its structure and its sentences", got)
	}
}

// The help shows the grammar every refusal repeats, not a copy of its own
// that could drift from it.
func TestHelpShowsTheGrammarARefusalRepeats(t *testing.T) {
	stdout, _, code := capture(t, "", func() int { return Run([]string{"help"}) })
	if code != exitOK || !strings.Contains(stdout, policy.Grammar) {
		t.Fatalf("exit = %d; help does not show policy.Grammar:\n%s", code, stdout)
	}
}
