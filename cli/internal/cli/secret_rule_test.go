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
)

const (
	ruleJSON = `{"id":"rule_1","projectId":"project-1","wellKnownId":"com.github.api","hosts":["github.com"],"purpose":"use",` +
		`"repositories":["github.com/org/repo"],"uses":["read any issue <number> in org/repo","push a branch discobox/<slug>; never main"],` +
		`"maxGrantTTLSeconds":3600,"enabled":true,"createdBy":"user-1","createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:01Z"}`
	pendingAskJSON = `{"id":"request-1","projectId":"project-1","requestedBy":"agent:sbx_1","sandboxId":"sbx_1","type":"token","hosts":["github.com"],` +
		`"status":"pending","wellKnownId":"com.github.api","uses":[{"description":"gh api GET repos/org/repo/issues/43"}],` +
		`"createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:01Z"}`
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
				`"uses":{"uses":["read any issue <number> in org/repo\u001b[2J"],"covers":false,"reason":"Refused: Jev could not tell (0.41)."}}`))
		default:
			t.Errorf("unexpected request = %s", key)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A rule is made directly with the sentences a person wrote, for the
// repositories they named. The CLI sends what was said and nothing it was not:
// a cap or a purpose nobody named is the server's default to give, and a
// repository is the server's to read and store.
func TestSecretRuleCreateSendsTheSentencesAsWritten(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "rule", "create", "--well-known", "com.github.api",
		"--repo", " git@github.com:org/repo.git ,github.com/org/other", "--repo", "https://gitlab.example.com/group/project",
		"--use", "read any issue <number> in org/repo", "--use", "push a branch discobox/<slug>; never main",
		"--max-grant-ttl", "2h")
	body := sent["POST /projects/project-1/approval-rules"]
	uses, _ := body["uses"].([]any)
	repositories, _ := body["repositories"].([]any)
	if body["wellKnownId"] != "com.github.api" || len(repositories) != 3 || repositories[0] != "git@github.com:org/repo.git" ||
		repositories[1] != "github.com/org/other" || repositories[2] != "https://gitlab.example.com/group/project" ||
		len(uses) != 2 || uses[1] != "push a branch discobox/<slug>; never main" || body["maxGrantTTLSeconds"] != float64(7200) {
		t.Fatalf("create body = %#v", body)
	}
	for _, unsaid := range []string{"purpose", "secretId", "enabled", "expiresAt", "hosts", "everyDiscobox"} {
		if _, ok := body[unsaid]; ok {
			t.Fatalf("create body = %#v, want %s left to the server", body, unsaid)
		}
	}
	// The rule's text is what approves, so it is read back whole.
	for _, want := range []string{"rule_1", "enabled", "com.github.api", "never main", "discoboxes of github.com/org/repo", "1 hour"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q in it", out, want)
		}
	}

	// A rule for every discobox is made by saying so, and names no repository.
	runCLI(t, server.URL, "secret", "rule", "create", "--well-known", "com.github.api", "--every-discobox", "--use", "read issues")
	if body := sent["POST /projects/project-1/approval-rules"]; body["repositories"] != nil || body["everyDiscobox"] != true {
		t.Fatalf("create body = %#v, want every discobox said in so many words, and no repository named", body)
	}
}

// A rule names one credential, says which discoboxes it is for and what it
// approves; the CLI refuses the others before it asks the server anything. A
// rule for every discobox is never what saying nothing makes.
func TestSecretRuleCreateRefusesWhatIsNotARule(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"no credential":                    {"--repo", "github.com/org/repo", "--use", "read issues"},
		"two credentials":                  {"--repo", "github.com/org/repo", "--well-known", "com.github.api", "--secret", "sec_1", "--use", "read issues"},
		"no uses":                          {"--repo", "github.com/org/repo", "--well-known", "com.github.api"},
		"an expiry of zero":                {"--repo", "github.com/org/repo", "--well-known", "com.github.api", "--use", "read issues", "--expires-in", "forever"},
		"nothing said of which discoboxes": {"--well-known", "com.github.api", "--use", "read issues"},
		"a repository that says nothing":   {"--repo", " ", "--well-known", "com.github.api", "--use", "read issues"},
		"repositories and every discobox":  {"--repo", "github.com/org/repo", "--every-discobox", "--well-known", "com.github.api", "--use", "read issues"},
	} {
		cmd := NewRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "secret", "rule", "create"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Errorf("a rule with %s was sent", name)
		}
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
		"a repository that says nothing":  {"--repo", ""},
		"repositories and every discobox": {"--repo", "github.com/org/repo", "--every-discobox"},
	} {
		delete(sent, "PATCH /projects/project-1/approval-rules/rule_1")
		cmd := NewRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "secret", "rule", "update", "rule_1"}, args...))
		if err := cmd.Execute(); err == nil || len(sent["PATCH /projects/project-1/approval-rules/rule_1"]) != 0 {
			t.Errorf("an edit with %s was sent: %v", name, err)
		}
	}
}

func TestSecretRuleListGetAndDelete(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	if out := runCLI(t, server.URL, "secret", "rule", "ls"); !strings.Contains(out, "rule_1") || !strings.Contains(out, "(+1 more)") || !strings.Contains(out, "github.com/org/repo") {
		t.Fatalf("ls = %q, want the rule, its repository and how many uses it has", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "ls", "--quiet"); strings.TrimSpace(out) != "rule_1" {
		t.Fatalf("ls --quiet = %q, want the ID alone", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "get", "rule_1"); !strings.Contains(out, "never main") || !strings.Contains(out, "discoboxes of github.com/org/repo") {
		t.Fatalf("get = %q, want every use, and which discoboxes it is for", out)
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
// else said it is the narrowest rule: the uses as asked, for the repository of
// the discobox that asked, which the server names — the CLI says nothing of
// which discoboxes, and the server never reads that as every one of them. The
// rule made is read back whole.
func TestSecretRequestApproveAutoLeavesTheRepositoryToTheServerUnlessTold(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto")
	rule, _ := sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	if rule == nil || len(rule) != 0 {
		t.Fatalf("rule = %#v, want a rule with nothing said: the asking discobox's repository, and the uses as asked", rule)
	}
	if !strings.Contains(out, "Approval rule made:") || !strings.Contains(out, "never main") {
		t.Fatalf("output = %q, want the rule that was made, in full", out)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto",
		"--rule-repo", "github.com/org/repo,github.com/org/other", "--rule-use", "read any issue <number> in org/repo")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	uses, _ := rule["uses"].([]any)
	repositories, _ := rule["repositories"].([]any)
	if len(repositories) != 2 || repositories[1] != "github.com/org/other" || rule["everyDiscobox"] != nil || len(uses) != 1 {
		t.Fatalf("rule = %#v, want the repositories and sentences given", rule)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto", "--rule-every-discobox")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	if rule == nil || rule["everyDiscobox"] != true || rule["repositories"] != nil {
		t.Fatalf("rule = %#v, want every discobox said, and no repository", rule)
	}
}

// A rule's sentences without --auto would be dropped, and the request
// approved once with nobody told: it is refused instead. An ordinary approval
// sends no rule.
func TestSecretRequestApproveRuleFlagsNeedAuto(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"a repository without --auto":     {"--rule-repo", "github.com/org/repo"},
		"a use without --auto":            {"--rule-use", "read issues"},
		"a repository and every discobox": {"--auto", "--rule-repo", "github.com/org/repo", "--rule-every-discobox"},
		"a repository that says nothing":  {"--auto", "--rule-repo", " "},
		"every discobox without --auto":   {"--rule-every-discobox"},
	} {
		cmd := NewRootCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "approve", "request-1"}, args...))
		if err := cmd.Execute(); err == nil {
			t.Errorf("%s was approved", name)
		}
	}
	if _, approved := sent["POST /projects/project-1/secret-requests/request-1/approve"]; approved {
		t.Fatal("a refused approval reached the server")
	}
	runCLI(t, server.URL, "secret", "request", "approve", "request-1")
	if _, has := sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"]; has {
		t.Fatal("an ordinary approval sent a rule")
	}
}

// Proposals are sentences a model wrote from an agent's ask. Each is printed
// whole with what the judge made of it, a failed one is said to be failed,
// and what it carries is escaped rather than sent to the terminal.
func TestSecretRequestProposeShowsEveryProposalAndItsVerdict(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "request", "propose", "request-1")
	for _, want := range []string{
		"com.github.api", "discoboxes of github.com/org/repo",
		"--rule-repo github.com/org/repo", "--rule-every-discobox",
		"FAILED", "Jev could not tell (0.41)",
		`--rule-use "read any issue <number> in org/repo`,
		"the same work on any target of its kind",
		"with no --rule-use, the rule approves the uses as asked",
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
		t.Fatalf("propose = %q, want the judge's refusal said as the judge's", out)
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
// with an escape sequence shown rather than obeyed, and with the repository,
// the levels and each verdict as the server said them.
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
	if proposals.Credential != "com.github.api" || proposals.Delegate || proposals.Written == nil {
		t.Fatalf("proposals = %+v", proposals)
	}
	if strings.ContainsRune(proposals.Repository, '\x1b') || !strings.HasPrefix(proposals.Repository, "github.com/org/repo") {
		t.Fatalf("repository = %q, want the asking discobox's, safe to draw", proposals.Repository)
	}
	if failed := proposals.Written; failed.Covers || !strings.Contains(failed.Reason, "0.41") {
		t.Fatalf("written = %+v, want it failed, with the judge's reason", failed)
	}
	written := proposals.Written.Uses[0]
	if strings.ContainsRune(written, '\x1b') || !strings.Contains(written, "read any issue <number> in org/repo") {
		t.Fatalf("use = %q, want its escape sequence shown as data", written)
	}
	// The server's own text is kept beside it, for a use left as written.
	if !strings.ContainsRune(proposals.Written.Stored[0], '\x1b') || proposals.RepositoryStored == proposals.Repository {
		t.Fatalf("stored = %q / %q, want the server's text kept", proposals.Written.Stored, proposals.RepositoryStored)
	}
}

// Every field the API takes on an edit has a flag, and each is sent as given:
// the uses and hosts replaced, the cap, the expiry, and the switch.
func TestSecretRuleUpdateCoversTheWholeBody(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	runCLI(t, server.URL, "secret", "rule", "update", "rule_1",
		"--repo", "github.com/org/repo", "--use", "read any issue in org/repo", "--use", "comment on any issue in org/repo",
		"--hosts", "github.com,api.github.com", "--max-grant-ttl", "2h", "--expires-in", "3d", "--enable")
	body := sent["PATCH /projects/project-1/approval-rules/rule_1"]
	repositories, _ := body["repositories"].([]any)
	uses, _ := body["uses"].([]any)
	hosts, _ := body["hosts"].([]any)
	expires, _ := body["expiresAt"].(string)
	if len(repositories) != 1 || len(uses) != 2 || len(hosts) != 2 || body["maxGrantTTLSeconds"] != float64(7200) ||
		expires == "" || body["enabled"] != true || len(body) != 6 {
		t.Fatalf("update body = %#v, want every field given and nothing else", body)
	}
}

// A rule made directly can say everything the API takes: a project secret
// rather than an ID, its hosts, to delegate, an expiry, and switched off.
func TestSecretRuleCreateCoversTheWholeBody(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	runCLI(t, server.URL, "secret", "rule", "create", "--secret", "secret_0123456789abcdef", "--every-discobox",
		"--use", "hand on read access to issues", "--hosts", "github.com", "--delegate", "--expires-in", "1w", "--disabled")
	body := sent["POST /projects/project-1/approval-rules"]
	hosts, _ := body["hosts"].([]any)
	expires, _ := body["expiresAt"].(string)
	if body["secretId"] != "secret_0123456789abcdef" || body["wellKnownId"] != nil || body["everyDiscobox"] != true ||
		len(hosts) != 1 || body["purpose"] != "delegate" || expires == "" || body["enabled"] != false {
		t.Fatalf("create body = %#v, want every field given", body)
	}
}

// Uses the server refused for their shape are said as that, never as the
// judge's reading: the judge was not asked.
func TestSecretRequestProposeSaysAShapeRefusalIsNotTheJudges(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /projects/project-1/secret-requests/request-1":
			_, _ = w.Write([]byte(pendingAskJSON))
		case "POST /projects/project-1/secret-requests/request-1/rule-proposals":
			_, _ = w.Write([]byte(`{"credential":"com.github.api","hosts":["github.com"],"purpose":"use",` +
				`"uses":{"uses":["read issues\\nand merge"],"covers":false,"refused":"each of a rule's uses is one sentence, on one line"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	out := runCLI(t, server.URL, "secret", "request", "propose", "request-1")
	if !strings.Contains(out, "these could not be a rule's, so the judge was not asked — each of a rule's uses is one sentence, on one line") ||
		strings.Contains(out, "the judge found these do not cover") {
		t.Fatalf("propose = %q, want the server's refusal said as its own", out)
	}
}
