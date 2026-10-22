package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// A stored rule's policy may be a model's, written from an agent's ask, and
// its repositories are whatever the server holds, and the console draws them
// on the card a person manages the rule from: they arrive with an escape
// sequence shown rather than obeyed, and with the rest of the rule as the
// server holds it.
func TestStoredRulesReachTheConsoleSafeToDraw(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if key := r.Method + " " + r.URL.Path; key != "GET /projects/project-1/approval-rules" {
			t.Errorf("unexpected request = %s", key)
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"approvalRules":[{"id":"rule_1","projectId":"project-1","wellKnownId":"com.github.api","secretId":"secret-1",` +
			`"repositories":["github.com/org/repo\u001b[2J","github.com/org/other"],"hosts":["github.com"],"purpose":"delegate",` +
			`"policy":` + policyJSON(map[string]string{"issue": "a GitHub issue\u001b]0;approved\u0007", "repository": "a GitHub repository"},
			`allow read on issue in repository $primary.repo`, `allow read on issue in repository "org/other"`) + `,"maxGrantTTLSeconds":3600,` +
			`"expiresAt":"2026-11-01T00:00:00Z","createdBy":"user:darren","enabled":false,` +
			`"createdAt":"2026-10-12T00:00:00Z","updatedAt":"2026-10-12T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
	rules, err := ds.Rules(context.Background(), "")
	if err != nil {
		t.Fatalf("Rules() error = %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("rules = %+v, want the one the server holds", rules)
	}
	rule := rules[0]
	if rule.Policy == nil || rule.Stored.Policy == nil {
		t.Fatalf("rule = %+v, want its policy", rule)
	}
	for _, text := range append(slices.Clone(rule.Repositories), rule.Policy.Definitions["issue"]) {
		if strings.ContainsAny(text, "\x1b\x07") {
			t.Fatalf("%q reached the console with a control character in it", text)
		}
	}
	if len(rule.Repositories) != 2 || !strings.HasPrefix(rule.Repositories[0], "github.com/org/repo") || len(rule.Policy.Statements) != 2 ||
		!strings.HasPrefix(rule.Policy.Definitions["issue"], "a GitHub issue") {
		t.Fatalf("repositories = %q, policy = %+v; want both kept, with what was escaped shown", rule.Repositories, rule.Policy)
	}
	// The server's own text is kept beside the drawn, for an edit to send
	// back for the lines it leaves alone.
	if !strings.Contains(rule.Stored.Policy.Definitions["issue"], "\x1b]0;approved\x07") ||
		!strings.Contains(rule.Stored.Repositories[0], "\x1b[2J") {
		t.Fatalf("stored = %+v, want the server's text as it holds it", rule.Stored)
	}
	if rule.ID != "rule_1" || rule.WellKnownID != "com.github.api" || rule.SecretID != "secret-1" || !rule.Delegate || rule.Enabled ||
		rule.MaxGrantTTL != time.Hour || rule.Expires.IsZero() || rule.CreatedBy != "user:darren" || rule.Created.IsZero() {
		t.Fatalf("rule = %+v, want it as the server holds it", rule)
	}
}

// Turning a rule off is an edit of one field: anything else in the body would
// be the server judging, or replacing, sentences nobody changed. Deleting one
// and listing the grants it minted each name the rule and nothing more.
func TestTheConsoleChangesARuleByItsIDAndSendsOnlyWhatChanges(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}

	if err := ds.SetRuleEnabled(context.Background(), "", "rule_1", false); err != nil {
		t.Fatalf("SetRuleEnabled() error = %v", err)
	}
	if edit := sent["PATCH /projects/project-1/approval-rules/rule_1"]; len(edit) != 1 || edit["enabled"] != false {
		t.Fatalf("edit sent %v, want only that it is disabled", edit)
	}

	grants, err := ds.Grants(context.Background(), "", tui.GrantFilter{RuleID: "rule_1"})
	if err != nil {
		t.Fatalf("Grants() error = %v", err)
	}
	asked := sent["GET /projects/project-1/secret-grants"]
	if asked["ruleId"] != "rule_1" || asked["secretId"] != "" {
		t.Fatalf("grants were asked for with %v, want the rule alone", asked)
	}
	if len(grants) != 1 || grants[0].RuleID != "rule_1" || grants[0].ScopeKey != "sbx_1" {
		t.Fatalf("grants = %+v, want the one the rule minted, naming it", grants)
	}

	if err := ds.DeleteRule(context.Background(), "", "rule_1"); err != nil {
		t.Fatalf("DeleteRule() error = %v", err)
	}
}

// The console passes on which discoboxes a rule is for as the person said it.
// Every discobox reaches the server only as that answer: a rule that names no
// repository and did not say it is sent saying neither, which the server
// answers with the asking discobox's repository or a refusal, never with the
// wider rule. A rule's policy is sent only when the person changed it: left
// out, it is the request's as asked.
func TestTheConsoleSaysEveryDiscoboxOnlyWhenThePersonDid(t *testing.T) {
	for name, tc := range map[string]struct {
		rule tui.ApprovalRule
		want map[string]any
	}{
		"a repository":   {tui.ApprovalRule{Repositories: []string{"github.com/org/repo"}}, map[string]any{"repositories": []any{"github.com/org/repo"}}},
		"every discobox": {tui.ApprovalRule{EveryDiscobox: true}, map[string]any{"everyDiscobox": true}},
		"neither said":   {tui.ApprovalRule{}, map[string]any{}},
		"a policy": {tui.ApprovalRule{Policy: &tuiPolicy}, map[string]any{"policy": map[string]any{
			"statements": []any{map[string]any{"effect": "allow", "actions": []any{"read"}, "resourceType": "issue", "name": nil,
				"scope": map[string]any{"type": "repository", "name": "$primary.repo"}, "when": nil, "unless": nil}},
			"definitions": map[string]any{"issue": "a GitHub issue", "repository": "a GitHub repository"},
		}}},
	} {
		t.Run(name, func(t *testing.T) {
			sent := map[string]map[string]any{}
			server := ruleServer(t, sent)
			client, err := apiclientgen.NewClient(server.URL)
			if err != nil {
				t.Fatalf("new client: %v", err)
			}
			ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}
			rule := tc.rule
			if err := ds.ApproveCredentialRequest(context.Background(), "", tui.Approval{RequestID: "request-1", SecretID: "secret-1", TTLSeconds: 3600, Rule: &rule}); err != nil {
				t.Fatalf("ApproveCredentialRequest() error = %v", err)
			}
			got, _ := sent["POST /projects/project-1/secret-requests/request-1/approve"]["rule"].(map[string]any)
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rule sent = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// tuiPolicy is a policy the console sends with a rule.
var tuiPolicy = func() policy.Policy {
	statements, err := policy.ParseStatements([]string{"allow read on issue in repository $primary.repo"})
	if err != nil {
		panic(err)
	}
	return policy.Policy{Statements: statements, Definitions: map[string]string{"issue": "a GitHub issue", "repository": "a GitHub repository"}}
}()

// A rule made from the console is sent as it was chosen: a well-known ID
// alone, the cap always (zero is the secret's own limit), and every discobox
// only in so many words. An edit sends what changed and nothing else.
func TestTheConsoleMakesAndEditsARuleWithWhatWasChosen(t *testing.T) {
	sent := map[string]map[string]any{}
	server := ruleServer(t, sent)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ds := &apiDataSource{app: &App{}, client: client, projectID: "project-1"}

	lapses := time.Now().Add(72 * time.Hour)
	made, err := ds.CreateRule(context.Background(), "", tui.NewRule{
		WellKnownID: "com.github.api", Hosts: []string{"github.com", "githubcopilot.com"},
		ApprovalRule: tui.ApprovalRule{Repositories: []string{"github.com/org/repo"}, Policy: &tuiPolicy},
		Expires:      lapses,
	})
	if err != nil || made.ID != "rule_1" {
		t.Fatalf("CreateRule() = %+v, %v", made, err)
	}
	body := sent["POST /projects/project-1/approval-rules"]
	if hosts, _ := body["hosts"].([]any); len(hosts) != 2 || hosts[1] != "githubcopilot.com" {
		t.Fatalf("create body = %#v, want the hosts the card read", body)
	}
	if body["wellKnownId"] != "com.github.api" || body["secretId"] != nil || body["everyDiscobox"] != nil || len(sentPolicy(t, body).Statements) != 1 ||
		body["maxGrantTTLSeconds"] != float64(0) || body["expiresAt"] == nil {
		t.Fatalf("create body = %#v", body)
	}
	if _, err := ds.CreateRule(context.Background(), "", tui.NewRule{
		SecretID: "secret-1", ApprovalRule: tui.ApprovalRule{EveryDiscobox: true, Policy: &tuiPolicy}, MaxGrantTTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}
	body = sent["POST /projects/project-1/approval-rules"]
	if body["secretId"] != "secret-1" || body["everyDiscobox"] != true || body["repositories"] != nil || body["maxGrantTTLSeconds"] != float64(3600) || body["expiresAt"] != nil {
		t.Fatalf("create body = %#v", body)
	}

	capped := 2 * time.Hour
	if err := ds.UpdateRule(context.Background(), "", "rule_1", tui.RuleChange{Policy: &tuiPolicy, MaxGrantTTL: &capped, NeverExpires: true}); err != nil {
		t.Fatal(err)
	}
	edit := sent["PATCH /projects/project-1/approval-rules/rule_1"]
	if len(edit) != 3 || edit["maxGrantTTLSeconds"] != float64(7200) || edit["neverExpires"] != true || edit["policy"] == nil {
		t.Fatalf("edit body = %#v, want the policy, the cap and the expiry cleared, and nothing else", edit)
	}
	if err := ds.UpdateRule(context.Background(), "", "rule_1", tui.RuleChange{EveryDiscobox: true}); err != nil {
		t.Fatal(err)
	}
	if edit = sent["PATCH /projects/project-1/approval-rules/rule_1"]; len(edit) != 1 || edit["everyDiscobox"] != true {
		t.Fatalf("edit body = %#v, want every discobox alone", edit)
	}
}
