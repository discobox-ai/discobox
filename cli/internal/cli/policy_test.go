package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apipolicy"
)

// askedPolicyJSON is a policy as the server answers a read of the request
// that asks for it: its variables resolved for the discobox that asked.
const askedPolicyJSON = `{"statements":[` +
	`{"effect":"allow","actions":["push"],"resourceType":"branch","name":"discobox/issue-46","scope":{"type":"repository","name":"$primary.repo"},"when":null,"unless":null},` +
	`{"effect":"deny","actions":["delete"],"resourceType":"anything","name":null,"scope":{"type":"anywhere"},"when":null,"unless":null},` +
	`{"effect":"allow","actions":["create","update"],"resourceType":"pull request","name":null,"scope":{"type":"repository","name":"$primary.repo"},"when":"its base is main","unless":null}],` +
	`"definitions":{"push":"send commits to a branch","branch":"a Git branch","pull request":"a GitHub pull request","repository":"a GitHub repository"},` +
	`"resolved":{"$primary.repo":"discobox-ai/discobox"}}`

const policyRequestJSON = `{"id":"request-1","projectId":"project-1","requestedBy":"agent:sbx-1","sandboxId":"sbx-1","type":"token","status":"pending",` +
	`"name":"github","uses":[{"description":"open a pull request"}],"policy":` + askedPolicyJSON + `,` +
	`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`

// A request that asks for a policy shows it as the person approving it reads
// it: grouped by scope, the variable followed by its value, the deny that
// applies anywhere once under "global", the condition under its statement,
// and every word defined where it is first used.
func TestSecretRequestGetRendersItsPolicy(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests/request-1" {
			_, _ = w.Write([]byte(policyRequestJSON))
			return
		}
		t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "get", "request-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute secret request get: %v", err)
	}
	var policy []string
	for _, row := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(row, "POLICY") || (len(policy) > 0 && strings.HasPrefix(row, " ")) {
			policy = append(policy, strings.TrimRight(row, " "))
		} else if len(policy) > 0 {
			break
		}
	}
	want := []string{
		"POLICY        in repository (a GitHub repository) $primary.repo (discobox-ai/discobox)",
		`                allow push (send commits to a branch) on branch (a Git branch) "discobox/issue-46"`,
		"                allow create, update on pull request (a GitHub pull request)",
		`                  when "its base is main"`,
		"              global",
		"                deny delete on anything",
	}
	if !reflect.DeepEqual(policy, want) {
		t.Fatalf("policy rows:\n%s\nwant:\n%s\nwhole output:\n%s", strings.Join(policy, "\n"), strings.Join(want, "\n"), out.String())
	}
}

// --policy-as-asked approves the request's own policy, read from it, so what
// is granted is what `get` showed; without it approving stays one call, and
// sends no policy for the server to refuse.
func TestSecretRequestApprovePolicyAsAskedSendsTheRequestsPolicy(t *testing.T) {
	var approved map[string]any
	var calls []string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests/request-1":
			_, _ = w.Write([]byte(policyRequestJSON))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secret-requests/request-1/approve":
			approved = nil
			if err := json.NewDecoder(r.Body).Decode(&approved); err != nil {
				t.Fatalf("decode approve body: %v", err)
			}
			_, _ = w.Write([]byte(strings.Replace(policyRequestJSON, `"status":"pending"`, `"status":"approved"`, 1)))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	run := func(args ...string) {
		t.Helper()
		calls = nil
		cmd := NewRootCommand()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "approve", "request-1"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute secret request approve %v: %v", args, err)
		}
	}

	run("--policy-as-asked")
	var want map[string]any
	if err := json.Unmarshal([]byte(askedPolicyJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(approved["policy"], want) {
		t.Fatalf("approved policy = %#v, want the request's own %#v", approved["policy"], want)
	}

	run()
	if _, sent := approved["policy"]; sent || len(calls) != 1 {
		t.Fatalf("without --policy-as-asked: calls %q, body %#v; want one call sending no policy", calls, approved)
	}
}

// --policy-as-asked on a request that asks for none says so, rather than
// approve something other than what was said.
func TestSecretRequestApprovePolicyAsAskedNeedsAPolicy(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests/request-1" {
			_, _ = w.Write([]byte(`{"id":"request-1","projectId":"project-1","requestedBy":"agent:sbx-1","type":"token","status":"pending","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
			return
		}
		t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "approve", "request-1", "--policy-as-asked"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "asks for no policy") {
		t.Fatalf("err = %v, want it to say the request asks for no policy", err)
	}
}

// The grant listing follows its table with each grant's policy, under the
// grant it is on.
func TestSecretGrantListRendersEachPolicy(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-grants" {
			_, _ = w.Write([]byte(`{"secretGrants":[` +
				`{"id":"grant-plain","projectId":"project-1","secretId":"secret-1","scope":"project","scopeKey":"project-1","hosts":["api.github.com"],"purpose":"use","grantedAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"},` +
				`{"id":"grant-policy","projectId":"project-1","secretId":"secret-1","scope":"sandbox","scopeKey":"sbx-1","hosts":["api.github.com"],"purpose":"use","policy":` + askedPolicyJSON + `,"grantedAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}]}`))
			return
		}
		t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "grant", "ls"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute secret grant ls: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "POLICY OF grant-plain") {
		t.Fatalf("a grant with no policy was given one:\n%s", got)
	}
	_, block, ok := strings.Cut(got, "\nPOLICY OF grant-policy\n")
	if !ok || !strings.HasPrefix(block, "  in repository (a GitHub repository) $primary.repo (discobox-ai/discobox)\n") ||
		!strings.Contains(block, "\n    deny delete on anything\n") {
		t.Fatalf("grant listing lacks grant-policy's rendered policy:\n%s", got)
	}
}

// The window's request carries the policy as the server sent it, and the
// approval sends it back unchanged: what is approved is what was read.
func TestTheWindowsRequestKeepsItsPolicyAsSent(t *testing.T) {
	var read apimodel.SecretRequest
	if err := read.UnmarshalJSON([]byte(policyRequestJSON)); err != nil {
		t.Fatal(err)
	}
	req := toTUICredentialRequest(read)
	if req.Policy == nil || len(req.PolicyLines) == 0 {
		t.Fatalf("request = %#v, want its policy and its rendering", req)
	}
	sent, err := apipolicy.To(*req.Policy)
	if err != nil {
		t.Fatal(err)
	}
	data, err := sent.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(askedPolicyJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sent back %s, want %s", data, askedPolicyJSON)
	}
}

// Text a discobox wrote reaches the window escaped in the form the window
// draws: a definition in a policy token's note, and the raw request's JSON,
// whose encoder would pass a C1 control through.
func TestTheWindowsRequestIsEscapedWhereItIsDrawn(t *testing.T) {
	hostile := strings.Replace(policyRequestJSON, `"send commits to a branch"`, `"send commits\u001b]8;;https://evil\u0007 to a branch"`, 1)
	hostile = strings.Replace(hostile, `"name":"github"`, `"name":"github","justification":"read \u009b31m this"`, 1)
	var read apimodel.SecretRequest
	if err := read.UnmarshalJSON([]byte(hostile)); err != nil {
		t.Fatal(err)
	}
	req := toTUICredentialRequest(read)
	for _, l := range req.PolicyLines {
		for _, tok := range l.Tokens {
			if strings.ContainsAny(tok.Text+tok.Note, "\x1b\x07") {
				t.Fatalf("token drawn raw: %+v", tok)
			}
		}
	}
	if strings.ContainsRune(req.Raw, '\u009b') || strings.ContainsRune(req.Raw, '\x1b') {
		t.Fatalf("raw request carries a control:\n%q", req.Raw)
	}
	if req.Policy == nil || !strings.Contains(req.Policy.Definitions["push"], "\x1b") {
		t.Fatal("the policy sent back on approval was changed; only what is drawn is escaped")
	}
}
