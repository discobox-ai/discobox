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
		`"scope":"discoboxes started to deliver an issue in org/repo","uses":["read any issue <number> in org/repo","push a branch discobox/<slug>; never main"],` +
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
				`"scopes":[{"level":1,"sandboxId":"sbx_1","covers":true,"reason":"this discobox, by its ID; nothing is read"},` +
				`{"level":2,"scope":"discoboxes started to deliver an issue in org/repo","covers":false,"reason":"Refused: Jev could not tell (0.41)."}],` +
				`"contents":[{"level":1,"uses":["gh api GET repos/org/repo/issues/43"],"covers":true,"reason":"within"},` +
				`{"level":2,"uses":["read any issue <number> in org/repo\u001b[2J"],"covers":true,"reason":"within"}]}`))
		default:
			t.Errorf("unexpected request = %s", key)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A rule is made directly with the sentences a person wrote. The CLI sends
// what was said and nothing it was not: a cap, a purpose or a pin nobody
// named is the server's default to give.
func TestSecretRuleCreateSendsTheSentencesAsWritten(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "rule", "create", "--well-known", "com.github.api",
		"--scope", " discoboxes started to deliver an issue in org/repo ",
		"--use", "read any issue <number> in org/repo", "--use", "push a branch discobox/<slug>; never main",
		"--max-grant-ttl", "2h")
	body := sent["POST /projects/project-1/approval-rules"]
	uses, _ := body["uses"].([]any)
	if body["wellKnownId"] != "com.github.api" || body["scope"] != "discoboxes started to deliver an issue in org/repo" ||
		len(uses) != 2 || uses[1] != "push a branch discobox/<slug>; never main" || body["maxGrantTTLSeconds"] != float64(7200) {
		t.Fatalf("create body = %#v", body)
	}
	for _, unsaid := range []string{"purpose", "sandboxId", "secretId", "enabled", "expiresAt", "hosts"} {
		if _, ok := body[unsaid]; ok {
			t.Fatalf("create body = %#v, want %s left to the server", body, unsaid)
		}
	}
	// The rule's text is what approves, so it is read back whole.
	for _, want := range []string{"rule_1", "enabled", "com.github.api", "never main", "discoboxes started to deliver", "1 hour"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q in it", out, want)
		}
	}
}

// A rule names one credential and says what it approves; the CLI refuses the
// others before it asks the server anything.
func TestSecretRuleCreateRefusesWhatIsNotARule(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"no credential":     {"--use", "read issues"},
		"two credentials":   {"--well-known", "com.github.api", "--secret", "sec_1", "--use", "read issues"},
		"no uses":           {"--well-known", "com.github.api"},
		"an expiry of zero": {"--well-known", "com.github.api", "--use", "read issues", "--expires-in", "forever"},
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
	runCLI(t, server.URL, "secret", "rule", "update", "rule_1", "--scope", "", "--never-expires", "--max-grant-ttl", "forever")
	body := sent["PATCH /projects/project-1/approval-rules/rule_1"]
	if len(body) != 3 || body["scope"] != "" || body["neverExpires"] != true || body["maxGrantTTLSeconds"] != float64(0) {
		t.Fatalf("update body = %#v, want the cleared scope, the cleared expiry and the zero cap, and nothing else", body)
	}
}

func TestSecretRuleListGetAndDelete(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	if out := runCLI(t, server.URL, "secret", "rule", "ls"); !strings.Contains(out, "rule_1") || !strings.Contains(out, "(+1 more)") {
		t.Fatalf("ls = %q, want the rule and how many uses it has", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "ls", "--quiet"); strings.TrimSpace(out) != "rule_1" {
		t.Fatalf("ls --quiet = %q, want the ID alone", out)
	}
	if out := runCLI(t, server.URL, "secret", "rule", "get", "rule_1"); !strings.Contains(out, "never main") {
		t.Fatalf("get = %q, want every use", out)
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
// else said it is the narrowest rule, which needs no model: pinned to the
// discobox that asked, the uses as asked. The rule made is read back whole.
func TestSecretRequestApproveAutoPinsTheRuleUnlessTold(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	out := runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto")
	rule, _ := sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	if rule == nil || rule["sandboxId"] != "sbx_1" || rule["scope"] != nil || rule["uses"] != nil {
		t.Fatalf("rule = %#v, want it pinned to the asking discobox with the uses left as asked", rule)
	}
	if !strings.Contains(out, "Approval rule made:") || !strings.Contains(out, "never main") {
		t.Fatalf("output = %q, want the rule that was made, in full", out)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto",
		"--rule-scope", "any discobox on org/repo", "--rule-use", "read any issue <number> in org/repo")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	uses, _ := rule["uses"].([]any)
	if rule["scope"] != "any discobox on org/repo" || rule["sandboxId"] != nil || len(uses) != 1 {
		t.Fatalf("rule = %#v, want the sentences given and no pin", rule)
	}

	runCLI(t, server.URL, "secret", "request", "approve", "request-1", "--auto", "--rule-every-discobox")
	rule, _ = sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
	if rule == nil || rule["scope"] != nil || rule["sandboxId"] != nil {
		t.Fatalf("rule = %#v, want neither a sentence nor a pin", rule)
	}
}

// A rule's sentences without --auto would be dropped, and the request
// approved once with nobody told: it is refused instead. An ordinary approval
// sends no rule.
func TestSecretRequestApproveRuleFlagsNeedAuto(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	for name, args := range map[string][]string{
		"a scope without --auto":        {"--rule-scope", "any discobox on org/repo"},
		"a use without --auto":          {"--rule-use", "read issues"},
		"a scope and every discobox":    {"--auto", "--rule-scope", "any discobox", "--rule-every-discobox"},
		"a scope that says nothing":     {"--auto", "--rule-scope", " "},
		"every discobox without --auto": {"--rule-every-discobox"},
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
		"com.github.api", "pinned to sbx_1",
		`--rule-scope "discoboxes started to deliver an issue in org/repo"`,
		"FAILED", "Jev could not tell (0.41)",
		`--rule-use "gh api GET repos/org/repo/issues/43"`,
		"the same work on any target of its kind",
		"approve request-1 --auto",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("propose = %q, want %q in it", out, want)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("propose = %q, want a proposal's escape sequence shown as data", out)
	}
}

// A pending request says why each rule tested against it did not cover it,
// which is why it is waiting for a person.
func TestSecretRequestGetSaysWhyNoRuleCoveredIt(t *testing.T) {
	asked := strings.Replace(pendingAskJSON, `"status":"pending"`,
		`"status":"pending","ruleTests":[{"ruleId":"rule_1","covered":false,"stage":"content","reason":"pushes to main"},`+
			`{"ruleId":"rule_2","covered":false,"pending":true,"stage":"scope"}]`, 1)
	server := completionServer(t, map[string]string{"/projects/project-1/secret-requests/request-1": asked})
	out := runCLI(t, server.URL, "secret", "request", "get", "request-1")
	for _, want := range []string{"rule_1 did not cover it (content): pushes to main", "rule_2: its scope is still being judged"} {
		if !strings.Contains(out, want) {
			t.Fatalf("get = %q, want %q in it", out, want)
		}
	}
}

// The console draws what a model wrote from an agent's ask: proposals reach it
// with an escape sequence shown rather than obeyed, and with the levels, the
// pin and each verdict as the server said them.
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
	if proposals.Credential != "com.github.api" || proposals.Delegate || len(proposals.Scopes) != 2 || len(proposals.Contents) != 2 {
		t.Fatalf("proposals = %+v", proposals)
	}
	if pin := proposals.Scopes[0]; pin.Level != 1 || pin.SandboxID != "sbx_1" || !pin.Covers {
		t.Fatalf("level 1 scope = %+v, want the pin", pin)
	}
	if failed := proposals.Scopes[1]; failed.Covers || !strings.Contains(failed.Reason, "0.41") {
		t.Fatalf("level 2 scope = %+v, want it failed, with the judge's reason", failed)
	}
	written := proposals.Contents[1].Uses[0]
	if strings.ContainsRune(written, '\x1b') || !strings.Contains(written, "read any issue <number> in org/repo") {
		t.Fatalf("use = %q, want its escape sequence shown as data", written)
	}
}
