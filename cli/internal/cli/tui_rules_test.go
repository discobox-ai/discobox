package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// A stored rule's scope and uses may be a model's sentences written from an
// agent's ask, and the console draws them on the card a person manages the
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
			`"scope":"discoboxes triaging org/repo\u001b[2J","hosts":["github.com"],"purpose":"delegate",` +
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
	for _, text := range append([]string{rule.Scope}, rule.Uses...) {
		if strings.ContainsAny(text, "\x1b\x07") {
			t.Fatalf("%q reached the console with a control character in it", text)
		}
	}
	if !strings.HasPrefix(rule.Scope, "discoboxes triaging org/repo") || len(rule.Uses) != 2 || !strings.HasPrefix(rule.Uses[0], "read any issue in org/repo") {
		t.Fatalf("scope = %q, uses = %q; want the sentences kept, with what was escaped shown", rule.Scope, rule.Uses)
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
