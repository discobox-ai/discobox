package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/policy"
)

// policyJSON is a policy as the API carries it, from its sentences and the
// definitions of their words.
func policyJSON(definitions map[string]string, sentences ...string) string {
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		panic(err)
	}
	data, err := json.Marshal(policy.Policy{Statements: statements, Definitions: definitions})
	if err != nil {
		panic(err)
	}
	return string(data)
}

var (
	ruleJSON = `{"id":"rule_1","projectId":"project-1","wellKnownId":"com.github.api","hosts":["github.com"],"purpose":"use",` +
		`"repositories":["github.com/org/repo"],"policy":` + policyJSON(map[string]string{"issue": "a GitHub issue", "repository": "a GitHub repository", "push": "send commits to it", "branch": "a git branch"},
		`allow read on issue in repository $primary.repo`, `deny push on branch "main" in anywhere`) +
		`,"maxGrantTTLSeconds":3600,"enabled":true,"createdBy":"user-1","createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:01Z"}`
	pendingAskJSON = `{"id":"request-1","projectId":"project-1","requestedBy":"agent:sbx_1","sandboxId":"sbx_1","type":"token","hosts":["github.com"],` +
		`"status":"pending","wellKnownId":"com.github.api","uses":[{"description":"gh api GET repos/org/repo/issues/43"}],` +
		`"policy":` + pendingPolicyJSON + `,` +
		`"createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:01Z"}`
	// pendingPolicyJSON is the policy the pending request asks for.
	pendingPolicyJSON = policyJSON(map[string]string{"issue": "a GitHub issue", "repository": "a GitHub repository"},
		`allow read on issue "43" in repository $primary.repo`)
)

// ruleServer answers the rule routes and records the bodies it was sent.
func ruleServer(t *testing.T, sent map[string]map[string]any) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		key := r.Method + " " + r.URL.Path
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil && r.ContentLength > 0 {
				t.Errorf("decode %s: %v", key, err)
			}
			sent[key] = body
		}
		switch key {
		case "GET /projects/project-1/approval-rules":
			_, _ = w.Write([]byte(`{"approvalRules":[` + ruleJSON + `]}`))
		case "POST /projects/project-1/approval-rules", "GET /projects/project-1/approval-rules/rule_1", "PATCH /projects/project-1/approval-rules/rule_1":
			_, _ = w.Write([]byte(ruleJSON))
		case "DELETE /projects/project-1/approval-rules/rule_1":
			w.WriteHeader(http.StatusNoContent)
		case "GET /projects/project-1/secret-requests/request-1":
			_, _ = w.Write([]byte(pendingAskJSON))
		case "POST /projects/project-1/secret-requests/request-1/approve":
			_, _ = w.Write([]byte(strings.Replace(pendingAskJSON, `"status":"pending"`, `"status":"approved","secretId":"secret-1","grantId":"grant_1"`, 1)))
		case "GET /projects/project-1/secret-grants":
			sent[key] = map[string]any{"ruleId": r.URL.Query().Get("ruleId"), "secretId": r.URL.Query().Get("secretId")}
			_, _ = w.Write([]byte(`{"secretGrants":[{"id":"grant_1","projectId":"project-1","secretId":"secret-1","scope":"sandbox","scopeKey":"sbx_1",` +
				`"purpose":"use","ruleId":"rule_1","grantedAt":"2026-10-12T00:00:00Z","createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:00Z"}]}`))
		case "POST /projects/project-1/secret-requests/request-1/rule-proposals":
			_, _ = w.Write([]byte(`{"credential":"com.github.api","hosts":["github.com"],"purpose":"use",` +
				`"repository":"github.com/org/repo\u001b[2J",` +
				`"policy":{"policy":` + proposedPolicyJSON + `,"covers":false,"reason":"Refused: Jev could not tell (0.41)."}}`))
		default:
			t.Errorf("unexpected request = %s", key)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// proposedPolicyJSON is the policy the project's judge writes from the
// pending request's, with an escape sequence in a definition a model wrote.
var proposedPolicyJSON = policyJSON(map[string]string{"issue": "a GitHub issue\u001b[2J", "repository": "a GitHub repository"},
	`allow read on issue in repository $primary.repo`)

// ruleFlags is a policy as the flags write one.
var ruleFlags = []string{
	"--statement", "allow read on issue in repository $primary.repo",
	"--statement", `deny push on branch "main" in anywhere`,
	"--define", "issue=a GitHub issue", "--define", "repository=a GitHub repository",
	"--define", "push=send commits to it", "--define", "branch=a git branch",
}

// sentPolicy is the policy a body sent, read back.
func sentPolicy(t *testing.T, body map[string]any) policy.Policy {
	t.Helper()
	data, err := json.Marshal(body["policy"])
	if err != nil {
		t.Fatal(err)
	}
	var p policy.Policy
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatalf("policy %s: %v", data, err)
	}
	return p
}

// A rule is made directly with the policy a person wrote, as discobox-access
// writes one, for the repositories they named. The CLI sends what was said
// and nothing it was not: a cap or a purpose nobody named is the server's
// default to give, and a repository is the server's to read and store.
func TestSecretRuleCreateSendsThePolicyAsWritten(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, append([]string{"secret", "rule", "create", "--well-known", "com.github.api",
		"--repo", " git@github.com:org/repo.git ,github.com/org/other", "--repo", "https://gitlab.example.com/group/project",
		"--max-grant-ttl", "2h"}, ruleFlags...)...)
	body := sent["POST /projects/project-1/approval-rules"]
	approves := sentPolicy(t, body)
	repositories, _ := body["repositories"].([]any)
	if body["wellKnownId"] != "com.github.api" || len(repositories) != 3 || repositories[0] != "git@github.com:org/repo.git" ||
		repositories[1] != "github.com/org/other" || repositories[2] != "https://gitlab.example.com/group/project" ||
		body["maxGrantTTLSeconds"] != float64(7200) {
		t.Fatalf("create body = %#v", body)
	}
	if sentences := policy.FormatStatements(approves.Statements); len(sentences) != 2 || sentences[1] != `deny push on branch "main" in anywhere` ||
		len(approves.Definitions) != 4 || approves.Definitions["push"] != "send commits to it" || approves.Resolved != nil {
		t.Fatalf("policy = %+v, want the statements and definitions given", approves)
	}
	for _, unsaid := range []string{"purpose", "secretId", "enabled", "expiresAt", "hosts", "everyDiscobox"} {
		if _, ok := body[unsaid]; ok {
			t.Fatalf("create body = %#v, want %s left to the server", body, unsaid)
		}
	}
	// The rule's text is what approves, so it is read back whole, in the
	// words a statement is written in.
	for _, want := range []string{"rule_1", "enabled", "com.github.api", "discoboxes of github.com/org/repo", "1 hour",
		"allow read on issue in repository $primary.repo", `deny push on branch "main" in anywhere`, `"push" means: send commits to it`,
		"$primary.repo, bound to whichever discobox asks"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q in it", out, want)
		}
	}

	// A rule for every discobox is made by saying so, and names no repository.
	runCLI(t, server.URL, append([]string{"secret", "rule", "create", "--well-known", "com.github.api", "--every-discobox"}, ruleFlags...)...)
	if body := sent["POST /projects/project-1/approval-rules"]; body["repositories"] != nil || body["everyDiscobox"] != true {
		t.Fatalf("create body = %#v, want every discobox said in so many words, and no repository named", body)
	}
}

// runCLIRefused runs a command that is refused, and returns why.
func runCLIRefused(t *testing.T, serverURL string, args ...string) string {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1"}, args...))
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("%v was not refused", args)
	}
	return err.Error()
}

// A rule names one credential, says which discoboxes it is for and what it
// approves; the CLI refuses the others before it asks the server anything. A
// rule for every discobox is never what saying nothing makes.
func TestSecretRuleCreateRefusesWhatIsNotARule(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"no credential":                    append([]string{"--repo", "github.com/org/repo"}, ruleFlags...),
		"two credentials":                  append([]string{"--repo", "github.com/org/repo", "--well-known", "com.github.api", "--secret", "sec_1"}, ruleFlags...),
		"no policy":                        {"--repo", "github.com/org/repo", "--well-known", "com.github.api"},
		"definitions alone":                {"--repo", "github.com/org/repo", "--well-known", "com.github.api", "--define", "issue=a GitHub issue"},
		"an expiry of zero":                append([]string{"--repo", "github.com/org/repo", "--well-known", "com.github.api", "--expires-in", "forever"}, ruleFlags...),
		"nothing said of which discoboxes": append([]string{"--well-known", "com.github.api"}, ruleFlags...),
		"a repository that says nothing":   append([]string{"--repo", " ", "--well-known", "com.github.api"}, ruleFlags...),
		"repositories and every discobox":  append([]string{"--repo", "github.com/org/repo", "--every-discobox", "--well-known", "com.github.api"}, ruleFlags...),
	} {
		t.Run(name, func(t *testing.T) {
			runCLIRefused(t, server.URL, append([]string{"secret", "rule", "create"}, args...)...)
		})
	}
	if len(sent) != 0 {
		t.Fatalf("the server was asked %v", sent)
	}
}

// A statement is parsed and the policy checked before anything is sent, and
// what is wrong is said with the grammar a statement is written in, as
// discobox-access says it to an agent.
func TestSecretRuleCreateSaysWhatIsWrongWithThePolicy(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	base := []string{"secret", "rule", "create", "--well-known", "com.github.api", "--repo", "github.com/org/repo"}
	said := runCLIRefused(t, server.URL, append(base, "--statement", "allow read issue in repository $primary.repo")...)
	if !strings.Contains(said, "a statement does not parse") || !strings.Contains(said, "a statement reads: allow|deny") || !strings.Contains(said, "--statement") {
		t.Fatalf("refusal = %q, want what does not parse, with the grammar", said)
	}
	said = runCLIRefused(t, server.URL, append(base, "--statement", "allow read on issue in repository $primary.repo", "--define", "repository=a GitHub repository")...)
	if !strings.Contains(said, `"issue" is not a default word and has no definition`) || !strings.Contains(said, "--define WORD=DEFINITION") {
		t.Fatalf("refusal = %q, want the word with no definition, and how to give one", said)
	}
	said = runCLIRefused(t, server.URL, append(base, "--statement", "allow read on issue in repository $primary.repo", "--define", "issue")...)
	if !strings.Contains(said, "WORD=DEFINITION") {
		t.Fatalf("refusal = %q, want the shape of a definition", said)
	}
	if len(sent) != 0 {
		t.Fatalf("the server was asked %v", sent)
	}
}

// An edit sends only what was given, and disabling is an edit: it mints
// nothing more and revokes nothing.
func TestSecretRuleUpdateSendsOnlyWhatWasGiven(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	runCLI(t, server.URL, "secret", "rule", "update", "rule_1", "--disable")
	if body := sent["PATCH /projects/project-1/approval-rules/rule_1"]; len(body) != 1 || body["enabled"] != false {
		t.Fatalf("update body = %#v, want only enabled: false", body)
	}
	runCLI(t, server.URL, "secret", "rule", "update", "rule_1", "--every-discobox", "--never-expires", "--max-grant-ttl", "forever")
	body := sent["PATCH /projects/project-1/approval-rules/rule_1"]
	if _, listed := body["repositories"]; len(body) != 3 || listed || body["everyDiscobox"] != true ||
		body["neverExpires"] != true || body["maxGrantTTLSeconds"] != float64(0) {
		t.Fatalf("update body = %#v, want every discobox said, the cleared expiry and the zero cap, and nothing else", body)
	}
	runCLI(t, server.URL, "secret", "rule", "update", "rule_1", "--repo", "github.com/org/other")
	body = sent["PATCH /projects/project-1/approval-rules/rule_1"]
	if repositories, _ := body["repositories"].([]any); len(body) != 1 || len(repositories) != 1 || repositories[0] != "github.com/org/other" {
		t.Fatalf("update body = %#v, want the repositories replaced and nothing else", body)
	}
	for name, args := range map[string][]string{
		"a repository that says nothing":     {"--repo", ""},
		"repositories and every discobox":    {"--repo", "github.com/org/repo", "--every-discobox"},
		"definitions without the statements": {"--define", "issue=a GitHub issue"},
		"a statement that does not parse":    {"--statement", "allow read issue"},
	} {
		delete(sent, "PATCH /projects/project-1/approval-rules/rule_1")
		runCLIRefused(t, server.URL, append([]string{"secret", "rule", "update", "rule_1"}, args...)...)
		if len(sent["PATCH /projects/project-1/approval-rules/rule_1"]) != 0 {
			t.Errorf("an edit with %s was sent", name)
		}
	}
}

func TestSecretRuleListGetAndDelete(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	if out := runCLI(t, server.URL, "secret", "rule", "ls"); !strings.Contains(out, "rule_1") || !strings.Contains(out, "allow read on issue in repository $primary.repo (+1 more)") ||
		!strings.Contains(out, "github.com/org/repo") {
		t.Fatalf("ls = %q, want the rule, its repository, its first statement and how many more it has", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "ls", "--quiet"); strings.TrimSpace(out) != "rule_1" {
		t.Fatalf("ls --quiet = %q, want the ID alone", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "get", "rule_1"); !strings.Contains(out, `deny push on branch "main" in anywhere`) ||
		!strings.Contains(out, `"branch" means: a git branch`) || !strings.Contains(out, "discoboxes of github.com/org/repo") {
		t.Fatalf("get = %q, want every statement and definition, and which discoboxes it is for", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "delete", "rule_1"); !strings.Contains(out, "rule_1") {
		t.Fatalf("delete = %q", out)
	}
	// The grants a rule minted outlive it, and are found by its ID.
	out := runCLI(t, server.URL, "secret", "grant", "ls", "--rule", "rule_1")
	if sent["GET /projects/project-1/secret-grants"]["ruleId"] != "rule_1" || !strings.Contains(out, "grant_1") {
		t.Fatalf("grant ls --rule = %q, asked %v", out, sent["GET /projects/project-1/secret-grants"])
	}
}

// --auto makes a rule from the request and approves under it. With nothing
// else said it is the narrowest rule: the policy as asked, for the
// repository of the discobox that asked, which the server names — the CLI
// says nothing of which discoboxes, and the server never reads that as every
// one of them. The approval itself names the request's policy as asked, as
// every approval of a request that asks for one does. The rule made is read
// back whole.
func TestSecretRequestApproveAutoLeavesTheRepositoryAndPolicyToTheServerUnlessTold(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto")
	approval := sent["POST /projects/project-1/secret-requests/request-1/approve"]
	rule, _ := approval["rule"].(map[string]any)
	if rule == nil || len(rule) != 0 {
		t.Fatalf("rule = %#v, want a rule with nothing said: the asking discobox's repository, and the policy as asked", rule)
	}
	if approved := sentPolicy(t, approval); len(approved.Statements) != 1 || policy.Format(approved.Statements[0]) != `allow read on issue "43" in repository $primary.repo` {
		t.Fatalf("approval policy = %+v, want the request's as asked", approved)
	}
	if !strings.Contains(out, "Approval rule made:") || !strings.Contains(out, `deny push on branch "main" in anywhere`) {
		t.Fatalf("output = %q, want the rule that was made, in full", out)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto",
		"--rule-repo", "github.com/org/repo,github.com/org/other", "--rule-statement", "allow read on issue in repository $primary.repo",
		"--rule-define", "issue=a GitHub issue", "--rule-define", "repository=a GitHub repository")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	repositories, _ := rule["repositories"].([]any)
	if approves := sentPolicy(t, rule); len(repositories) != 2 || repositories[1] != "github.com/org/other" || rule["everyDiscobox"] != nil ||
		len(approves.Statements) != 1 || approves.Definitions["issue"] != "a GitHub issue" {
		t.Fatalf("rule = %#v, want the repositories and the policy given", rule)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto", "--rule-every-discobox")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	if rule == nil || rule["everyDiscobox"] != true || rule["repositories"] != nil {
		t.Fatalf("rule = %#v, want every discobox said, and no repository", rule)
	}
}

// A rule's policy without --auto would be dropped, and the request approved
// once with nobody told: it is refused instead. An ordinary approval sends
// no rule.
func TestSecretRequestApproveRuleFlagsNeedAuto(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"a repository without --auto":         {"--rule-repo", "github.com/org/repo"},
		"a statement without --auto":          {"--rule-statement", "allow read on issue in repository $primary.repo"},
		"a definition without --auto":         {"--rule-define", "issue=a GitHub issue"},
		"a repository and every discobox":     {"--auto", "--rule-repo", "github.com/org/repo", "--rule-every-discobox"},
		"a repository that says nothing":      {"--auto", "--rule-repo", " "},
		"every discobox without --auto":       {"--rule-every-discobox"},
		"a statement that does not parse":     {"--auto", "--rule-statement", "allow read issue"},
		"definitions without their statement": {"--auto", "--rule-define", "issue=a GitHub issue"},
	} {
		t.Run(name, func(t *testing.T) {
			runCLIRefused(t, server.URL, append([]string{"secret", "request", "approve", "request-1"}, args...)...)
		})
	}
	if _, approved := sent["POST /projects/project-1/secret-requests/request-1/approve"]; approved {
		t.Fatal("a refused approval reached the server")
	}
	runCLI(t, server.URL, "secret", "request", "approve", "request-1")
	if _, has := sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"]; has {
		t.Fatal("an ordinary approval sent a rule")
	}
}

// A rule answers a request's policy, so --auto on a request of uses alone is
// refused before anything is approved, saying to approve it as a person.
func TestSecretRequestApproveAutoRefusesARequestOfUsesAlone(t *testing.T) {
	approved := false
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /projects/project-1/secret-requests/request-1":
			_, _ = w.Write([]byte(strings.Replace(pendingAskJSON, `"policy":`+pendingPolicyJSON+`,`, "", 1)))
		case "POST /projects/project-1/secret-requests/request-1/approve":
			approved = true
			http.Error(w, "unexpected", http.StatusTeapot)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if said := runCLIRefused(t, server.URL, "secret", "request", "approve", "request-1", "--auto"); !strings.Contains(said, "asks for uses alone") || approved {
		t.Fatalf("refusal = %q, approved = %v; want it refused before approving", said, approved)
	}
}

// A proposal is a policy a model wrote from an agent's ask. It is printed
// whole, as the flags that send it, with what holding it to the request
// found; a failed one is said to be failed, and what it carries is escaped
// rather than sent to the terminal.
func TestSecretRequestProposeShowsTheProposalAndWhatItFound(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "request", "propose", "request-1")
	for _, want := range []string{
		"com.github.api", "discoboxes of github.com/org/repo",
		"--rule-repo github.com/org/repo", "--rule-every-discobox",
		"FAILED", "Jev could not tell (0.41)",
		`--rule-statement 'allow read on issue in repository $primary.repo'`,
		`--rule-define 'issue=a GitHub issue\x1b[2J'`,
		"the same work on any target of its kind",
		"with no --rule-statement, the rule approves the policy as asked",
		"approve request-1 --auto",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("propose = %q, want %q in it", out, want)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("propose = %q, want a proposal's escape sequence shown as data", out)
	}
	if strings.Contains(out, "could not be a rule's") {
		t.Fatalf("propose = %q, want the finding said as a finding, not a refusal", out)
	}
}

// A pending request says why each rule tested against it did not cover it,
// which is why it is waiting for a person.
func TestSecretRequestGetSaysWhyNoRuleCoveredIt(t *testing.T) {
	asked := strings.Replace(pendingAskJSON, `"status":"pending"`,
		`"status":"pending","ruleTests":[{"ruleId":"rule_1","covered":false,"stage":"content","reason":"pushes to main"},`+
			`{"ruleId":"rule_2","covered":false,"pending":true,"stage":"content"},`+
			`{"ruleId":"rule_3","covered":false,"stage":"facts","reason":"the rule is for discoboxes of github.com/org/other, and this discobox is of github.com/org/repo"}]`, 1)
	server := completionServer(t, map[string]string{"/projects/project-1/secret-requests/request-1": asked})
	out := runCLI(t, server.URL, "secret", "request", "get", "request-1")
	for _, want := range []string{"rule_1 did not cover it (content): pushes to main", "rule_2: its content is still being judged",
		"rule_3 did not cover it (facts): the rule is for discoboxes of github.com/org/other"} {
		if !strings.Contains(out, want) {
			t.Fatalf("get = %q, want %q in it", out, want)
		}
	}
}

// The console draws what a model wrote from an agent's ask: proposals reach it
// with an escape sequence shown rather than obeyed, and with the repository
// and what holding the policy to the request found as the server said them.
func TestRuleProposalsReachTheConsoleSafeToDraw(t *testing.T) {
	server := ruleServer(t, map[string]map[string]any{})
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	proposals, err := ds.RuleProposals(context.Background(), "", "request-1")
	if err != nil {
		t.Fatalf("RuleProposals() error = %v", err)
	}
	if proposals.Credential != "com.github.api" || proposals.Delegate || proposals.Written == nil || proposals.Written.Policy == nil {
		t.Fatalf("proposals = %+v", proposals)
	}
	if strings.ContainsRune(proposals.Repository, '\x1b') || !strings.HasPrefix(proposals.Repository, "github.com/org/repo") {
		t.Fatalf("repository = %q, want the asking discobox's, safe to draw", proposals.Repository)
	}
	if failed := proposals.Written; failed.Covers || !strings.Contains(failed.Reason, "0.41") {
		t.Fatalf("written = %+v, want it failed, with the reason", failed)
	}
	written := proposals.Written.Policy.Definitions["issue"]
	if strings.ContainsRune(written, '\x1b') || !strings.HasPrefix(written, "a GitHub issue") {
		t.Fatalf("definition = %q, want its escape sequence shown as data", written)
	}
	// The server's own text is kept beside it, for a line left as written.
	if !strings.ContainsRune(proposals.Written.Stored.Definitions["issue"], '\x1b') || proposals.RepositoryStored == proposals.Repository {
		t.Fatalf("stored = %+v / %q, want the server's text kept", proposals.Written.Stored, proposals.RepositoryStored)
	}
}

// Every field the API takes on an edit has a flag, and each is sent as given:
// the policy and hosts replaced, the cap, the expiry, and the switch.
func TestSecretRuleUpdateCoversTheWholeBody(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	runCLI(t, server.URL, append([]string{"secret", "rule", "update", "rule_1",
		"--repo", "github.com/org/repo", "--hosts", "github.com,api.github.com", "--max-grant-ttl", "2h", "--expires-in", "3d", "--enable"}, ruleFlags...)...)
	body := sent["PATCH /projects/project-1/approval-rules/rule_1"]
	repositories, _ := body["repositories"].([]any)
	hosts, _ := body["hosts"].([]any)
	expires, _ := body["expiresAt"].(string)
	if len(repositories) != 1 || len(sentPolicy(t, body).Statements) != 2 || len(hosts) != 2 || body["maxGrantTTLSeconds"] != float64(7200) ||
		expires == "" || body["enabled"] != true || len(body) != 6 {
		t.Fatalf("update body = %#v, want every field given and nothing else", body)
	}
}

// A rule made directly can say everything the API takes: a project secret
// rather than an ID, its hosts, an expiry, and switched off. It approves asks
// to use a credential: no rule delegates yet, so nothing asks for one that
// does.
func TestSecretRuleCreateCoversTheWholeBody(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	runCLI(t, server.URL, "secret", "rule", "create", "--secret", "secret_0123456789abcdef", "--every-discobox",
		"--statement", "allow read on issue in repository $primary.repo", "--define", "issue=a GitHub issue", "--define", "repository=a GitHub repository",
		"--hosts", "github.com", "--expires-in", "1w", "--disabled")
	body := sent["POST /projects/project-1/approval-rules"]
	hosts, _ := body["hosts"].([]any)
	expires, _ := body["expiresAt"].(string)
	if body["secretId"] != "secret_0123456789abcdef" || body["wellKnownId"] != nil || body["everyDiscobox"] != true ||
		len(hosts) != 1 || body["purpose"] != nil || expires == "" || body["enabled"] != false {
		t.Fatalf("create body = %#v, want every field given", body)
	}
}

// What the server refused as no policy a rule could have is said as that,
// never as a finding about the request: it was not held to it. The
// statements are shown as written, and offered as no flag.
func TestSecretRequestProposeSaysARefusalIsNotAFinding(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /projects/project-1/secret-requests/request-1":
			_, _ = w.Write([]byte(pendingAskJSON))
		case "POST /projects/project-1/secret-requests/request-1/rule-proposals":
			_, _ = w.Write([]byte(`{"credential":"com.github.api","hosts":["github.com"],"purpose":"use",` +
				`"policy":{"statements":["allow merging of pull requests"],"covers":false,"refused":"statement 1 does not parse"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	out := runCLI(t, server.URL, "secret", "request", "propose", "request-1")
	if !strings.Contains(out, "this could not be a rule's policy, so it was not held to the request — statement 1 does not parse") ||
		!strings.Contains(out, "     allow merging of pull requests") || strings.Contains(out, "--rule-statement 'allow merging") ||
		strings.Contains(out, "it does not cover this request") {
		t.Fatalf("propose = %q, want the server's refusal said as its own", out)
	}
}
