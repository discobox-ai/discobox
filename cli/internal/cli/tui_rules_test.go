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
)

// A stored rule's uses may be a model's sentences written from an agent's ask,
// and its repositories are whatever the server holds, and the console draws
// them on the card a person manages the
// rule from: they arrive with an escape sequence shown rather than obeyed,
// and with the rest of the rule as the server holds it.
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
			`"uses":["read any issue in org/repo\u001b]0;approved\u0007","comment on it"],"maxGrantTTLSeconds":3600,` +
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
	for _, text := range append(slices.Clone(rule.Repositories), rule.Uses...) {
		if strings.ContainsAny(text, "\x1b\x07") {
			t.Fatalf("%q reached the console with a control character in it", text)
		}
	}
	if len(rule.Repositories) != 2 || !strings.HasPrefix(rule.Repositories[0], "github.com/org/repo") || len(rule.Uses) != 2 || !strings.HasPrefix(rule.Uses[0], "read any issue in org/repo") {
		t.Fatalf("repositories = %q, uses = %q; want both kept, with what was escaped shown", rule.Repositories, rule.Uses)
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
// wider rule.
func TestTheConsoleSaysEveryDiscoboxOnlyWhenThePersonDid(t *testing.T) {
	for name, tc := range map[string]struct {
		rule tui.ApprovalRule
		want map[string]any
	}{
		"a repository":   {tui.ApprovalRule{Repositories: []string{"github.com/org/repo"}}, map[string]any{"repositories": []any{"github.com/org/repo"}}},
		"every discobox": {tui.ApprovalRule{EveryDiscobox: true}, map[string]any{"everyDiscobox": true}},
		"neither said":   {tui.ApprovalRule{}, map[string]any{}},
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
