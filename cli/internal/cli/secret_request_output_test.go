package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
)

// A request's roles are shown grouped by document, never by name, each beside
// what its routes grant — paths with their wildcards marked — and the
// discobox's own words are marked as such and escaped (ADR 26-11-02-022 §3).
func TestARequestShowsItsRolesBesideWhatTheyGrant(t *testing.T) {
	const evil = "\x1b]0;pwned\a"
	pulls := apimodel.SecretRoute{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}, Paths: []string{"/repos/acme/web/pulls"}}
	wide := apimodel.SecretRoute{Hosts: []string{"api.github.com"}, Methods: []string{"POST"}, Paths: []string{"/**"}}
	policy := func(digest, repo string, unused []string, routes ...apimodel.SecretRoute) apimodel.SecretRequestPolicy {
		return apimodel.SecretRequestPolicy{
			File: "roles/github-submit.json", Name: "github-submit", Digest: digest,
			Description: apiclientgen.NewOptString("Open pull requests in one repository" + evil),
			Args:        apiclientgen.NewOptSecretRequestPolicyArgs(apiclientgen.SecretRequestPolicyArgs{"repo": repo}),
			UnusedArgs:  unused,
			Routes:      routes,
		}
	}
	request := &apimodel.SecretRequest{
		ID: "sreq_1", ProjectId: "project-1", RequestedBy: "agent:sbx_1", Status: apiclientgen.SecretRequestStatusPending,
		Use: apiclientgen.NewOptSecretRequestPart(apimodel.SecretRequestPart{
			Routes:   []apimodel.SecretRoute{pulls, pulls, wide},
			Policies: []apimodel.SecretRequestPolicy{policy("d1", "web", nil, pulls), policy("d1", "api", nil, pulls), policy("d2", "vm", []string{"repo"}, wide)},
		}),
		Delegate: apiclientgen.NewOptSecretRequestPart(apimodel.SecretRequestPart{
			Routes:   []apimodel.SecretRoute{wide},
			Policies: []apimodel.SecretRequestPolicy{policy("d2", "vm", []string{"repo"}, wide)},
		}),
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := (&App{}).writeSecretRequest(cmd, request, nil, nil); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"USE ROLE", "github-submit", "repo=web; repo=api", "(the discobox's words)",
		"POST · /repos/acme/web/pulls",
		"DELEGATE", "to give the credential to the discoboxes it leads",
		"/[**]",
		"also named \"github-submit\" but is a different document",
		"args not used by any route: repo",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
	if strings.ContainsAny(text, "\x1b\a") {
		t.Errorf("output carries the discobox's control characters raw:\n%q", text)
	}
	// One document asked twice is one role; two documents of one name are
	// two.
	if strings.Count(text, "USE ROLE") != 2 {
		t.Errorf("want a use role for each of two documents:\n%s", text)
	}
}

// The console's answer goes as one call naming a decision for each part,
// raw only on the use part (ADR 26-11-02-022 §4).
func TestTheConsoleAnswersEachPartInOneCall(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/secret-requests/sreq_1/approve" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"approved","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := &apiDataSource{app: &App{serverURL: server.URL}, client: client, projectID: "project-1"}
	err = d.ApproveCredentialRequest(t.Context(), "", tui.Approval{
		RequestID: "sreq_1", SecretID: "sec_1", TTLSeconds: 3600, Raw: "env",
		Use: tui.PartApprove, Delegate: tui.PartDeny,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("calls = %d, want one", len(bodies))
	}
	use, _ := bodies[0]["use"].(map[string]any)
	delegate, _ := bodies[0]["delegate"].(map[string]any)
	if use["decision"] != "approve" || use["ttlSeconds"] != float64(3600) || use["raw"] != "env" {
		t.Fatalf("use = %#v", use)
	}
	if delegate["decision"] != "deny" || len(delegate) != 1 {
		t.Fatalf("delegate = %#v, want a bare deny", delegate)
	}
}

// What approving a part does to what the discobox already holds is read
// before it is approved — `get --secret` — and said after: the binding kept,
// the ones that go, and one that only loses its use, by the rule the server
// applies (ADR 26-11-02-022 §5).
func TestARequestSaysWhatApprovingItReplaces(t *testing.T) {
	const requestID = "request-1"
	binding := func(id, env string, use, delegate bool, created string) string {
		return `{"id":"` + id + `","projectId":"project-1","secretId":"sec_1","targetKind":"sandbox","targetId":"sbx-1","use":` + fmt.Sprint(use) +
			`,"delegate":` + fmt.Sprint(delegate) + `,"envVar":"` + env + `","createdAt":"` + created + `","updatedAt":"` + created + `"}`
	}
	held := `{"secretBindings":[` + binding("bnd_old", "GITHUB_TOKEN", true, false, "2026-06-17T00:00:00Z") + `,` +
		binding("bnd_asked", "GH_TOKEN", true, false, "2026-06-17T00:01:00Z") + `,` +
		binding("bnd_both", "GH_LEAD", true, true, "2026-06-17T00:02:00Z") + `]}`
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests/"+requestID:
			_, _ = w.Write([]byte(pendingRequestJSON(requestID, "use")))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets":
			_, _ = w.Write([]byte(`{"secrets":[{"id":"sec_1","projectId":"project-1","name":"github","type":"token","maxBindingTTLSeconds":3600,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-bindings":
			if r.URL.Query().Get("target") != "sbx-1" || r.URL.Query().Get("targetKind") != "sandbox" {
				t.Errorf("bindings listed with %s, want the asking discobox's own", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(held))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/bound-secrets":
			// What applies in GH_TOKEN now: the discobox's own binding, for
			// good, and an addition stacking on it for an hour.
			_, _ = w.Write([]byte(`{"boundSecrets":[` +
				`{"bindingId":"bnd_asked","secretId":"sec_1","from":"sandbox","fromId":"sbx-1","use":true,"delegate":false,"envVar":"GH_TOKEN","routes":[[{"hosts":["api.github.com"]}],[{"hosts":["api.github.com"],"methods":["GET"]}]]},` +
				`{"bindingId":"bnd_added","secretId":"sec_1","from":"sandbox","fromId":"sbx-1","use":true,"delegate":false,"adds":true,"envVar":"GH_TOKEN","expiresAt":"2099-01-01T00:00:00Z","routes":[[{"hosts":["api.github.com"]}],[{"hosts":["api.github.com"],"methods":["POST"]}]]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "get", requestID, "--secret", "github"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get: %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"USE REPLACES", "bnd_asked in GH_TOKEN (sent to the secret's routes) is kept and takes the approval",
		"bnd_old in GITHUB_TOKEN (sent to the secret's routes) goes",
		"bnd_both in GH_LEAD (sent to the secret's routes) loses its use; its delegation stays",
		// What applies in the variable now, and what each way of granting
		// the use part would do (ADR 26-11-03-329 §5).
		"USE ASKED AS", "replace · the discobox's whole use of the credential",
		"USE HOLDS", "binding bnd_asked → GET api.github.com, with no expiry",
		"addition bnd_added → POST api.github.com, until",
		"USE NEW ROUTES", "api.github.com (compared as written, not as what they allow)",
		"USE AS ADD", "an addition: these routes for 1 hour; what it holds in GH_TOKEN stays as it is",
		"USE AS REPLACE (asked)",
		"IF REPLACED", "bnd_asked held with no expiry, takes the approval's routes and lasts only 1 hour",
		"bnd_added goes",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("get --secret lacks %q:\n%s", want, text)
		}
	}
}

// A part with nothing to keep says it makes a new binding, and the delegate
// part's split is said as one.
func TestAReplacementSaysANewBindingAndASplit(t *testing.T) {
	created := time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)
	both := apimodel.SecretBinding{ID: "bnd_both", Use: true, Delegate: true, EnvVar: apiclientgen.NewOptString("GH_LEAD"), CreatedAt: created}
	held := map[string]apimodel.SecretBinding{"bnd_both": both}
	standing := []bindingplan.Binding{{ID: "bnd_both", Use: true, Delegate: true, EnvName: "GH_LEAD", Created: created}}
	use := partReplacement{label: "use", plan: bindingplan.Use(standing, "GH_TOKEN"), held: held}
	if lines := strings.Join(use.lines(true), "\n"); !strings.Contains(lines, "lost its use") || !strings.Contains(lines, "a new binding was made") {
		t.Errorf("use lines:\n%s", lines)
	}
	delegate := partReplacement{label: "delegate", plan: bindingplan.Delegate(standing), held: held}
	if lines := strings.Join(delegate.lines(false), "\n"); !strings.Contains(lines, "stays the delegation; its use moves") || strings.Contains(lines, "new binding") {
		t.Errorf("delegate lines:\n%s", lines)
	}
	// Both approved: the delegate part reads what the use part left — the
	// delegation alone, updated, with one new use binding in all.
	usePlan, delegatePlan := bindingplan.Answer(standing, "GH_TOKEN", bindingplan.Replace, true)
	answered := strings.Join(append(partReplacement{label: "use", plan: usePlan, held: held}.lines(false),
		partReplacement{label: "delegate", plan: delegatePlan, held: held}.lines(false)...), "\n")
	if strings.Count(answered, "a new binding is made") != 1 || strings.Contains(answered, "moves") ||
		!strings.Contains(answered, "the delegation bnd_both (sent to the secret's routes) is kept and takes the approval") {
		t.Errorf("both parts:\n%s", answered)
	}
}

func TestTheConsoleSubmitsAnAdditionWithoutExpiry(t *testing.T) {
	var bodies []map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/secret-requests/sreq_1/approve" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"approved","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := &apiDataSource{app: &App{serverURL: server.URL}, client: client, projectID: "project-1"}
	err = d.ApproveCredentialRequest(t.Context(), "", tui.Approval{
		RequestID: "sreq_1", SecretID: "sec_1", TTLSeconds: 0, UseAs: "add",
		Use: tui.PartApprove, Delegate: tui.PartDeny,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 1 {
		t.Fatalf("calls = %d, want one", len(bodies))
	}
	use, _ := bodies[0]["use"].(map[string]any)
	delegate, _ := bodies[0]["delegate"].(map[string]any)
	if use["decision"] != "approve" || use["ttlSeconds"] != float64(0) || use["as"] != "add" {
		t.Fatalf("use = %#v", use)
	}
	if delegate["decision"] != "deny" || len(delegate) != 1 {
		t.Fatalf("delegate = %#v, want a bare deny", delegate)
	}
}

func TestApprovalHistoryDistinguishesEmptyGrantRestrictions(t *testing.T) {
	request := &apimodel.SecretRequest{Use: apiclientgen.NewOptSecretRequestPart(apimodel.SecretRequestPart{Grant: apiclientgen.NewOptSecretRequestGrant(apimodel.SecretRequestGrant{RouteLists: []apimodel.SecretRequestGrantRouteList{
		{Source: "secret", SourceId: "s"}, {Source: "binding", SourceId: "b"}, {Source: "parent", SourceId: "p"},
	}})})}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := (&App{}).writeSecretRequest(cmd, request, nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"USE GRANTED secret s", "nowhere", "USE GRANTED binding b", "No grant restriction", "USE GRANTED parent p"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Count(out.String(), "No grant restriction") != 2 {
		t.Fatalf("out=%s", out.String())
	}
}
