package access

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// The CLI is driven by an LLM, so what is asserted here is the contract an
// agent depends on: ids it can copy from one call into the next without
// scraping prose, and failures it can branch on without matching wording.

type fakeService struct {
	credentials  []agentcreds.Credential
	gotRequest   agentcreds.RequestBody
	status       agentcreds.RequestStatus
	requestPolls []agentcreds.RequestStatus
	requestIDs   []string
	trustIDs     []string
	statusErr    error
	trusts       []agentcreds.Trust
	gotTrust     agentcreds.TrustRequestBody
	trustStatus  agentcreds.TrustRequestStatus
	trustPolls   []agentcreds.TrustRequestStatus
	gotAudit     agentcreds.AuditQuery
	exchanges    []agentcreds.AuditExchange
	environment  map[string]string
	// requestErr is what Request answers with, as a service refusing the ask.
	requestErr error
}

func (f *fakeService) Environment(context.Context) (map[string]string, error) {
	return f.environment, nil
}

func (f *fakeService) List(context.Context) ([]agentcreds.Credential, error) {
	return f.credentials, nil
}

func (f *fakeService) Request(_ context.Context, body agentcreds.RequestBody) (agentcreds.RequestStatus, error) {
	f.gotRequest = body
	if f.requestErr != nil {
		return agentcreds.RequestStatus{}, f.requestErr
	}
	status := agentcreds.RequestStatus{RequestID: "sreq_1", Status: agentcreds.StatusPending}
	if body.Uses() {
		status.Use = &agentcreds.PartStatus{Status: agentcreds.StatusPending}
	}
	if body.Delegate != nil {
		status.Delegate = &agentcreds.PartStatus{Status: agentcreds.StatusPending}
	}
	return status, nil
}

func (f *fakeService) RequestStatus(_ context.Context, id string) (agentcreds.RequestStatus, error) {
	f.requestIDs = append(f.requestIDs, id)
	if f.statusErr != nil {
		return agentcreds.RequestStatus{}, f.statusErr
	}
	if len(f.requestPolls) > 0 {
		next := f.requestPolls[0]
		if len(f.requestPolls) > 1 {
			f.requestPolls = f.requestPolls[1:]
		}
		return next, nil
	}
	return f.status, nil
}

func (f *fakeService) Trusts(context.Context) ([]agentcreds.Trust, error) {
	return f.trusts, nil
}

func (f *fakeService) RequestTrust(_ context.Context, body agentcreds.TrustRequestBody) (agentcreds.TrustRequestStatus, error) {
	f.gotTrust = body
	return f.trustStatus, nil
}

func (f *fakeService) TrustRequestStatus(_ context.Context, id string) (agentcreds.TrustRequestStatus, error) {
	f.trustIDs = append(f.trustIDs, id)
	next := f.trustPolls[0]
	if len(f.trustPolls) > 1 {
		f.trustPolls = f.trustPolls[1:]
	}
	return next, nil
}

func (f *fakeService) AuditHTTP(_ context.Context, query agentcreds.AuditQuery) ([]agentcreds.AuditExchange, error) {
	f.gotAudit = query
	return f.exchanges, nil
}

// serve points the CLI at a real handler over HTTP, so these exercise the wire
// format rather than an in-process shortcut.
func serve(t *testing.T, svc agentcreds.Service) {
	t.Helper()
	server := httptest.NewServer(agentcreds.NewHandler(svc))
	t.Cleanup(server.Close)
	t.Setenv(agentcreds.URLEnv, server.URL)
}

// capture redirects the process streams for one command, which is what lets a
// test read exactly what an agent would.
func capture(t *testing.T, stdin string, run func() int) (stdout, stderr string, code int) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	inR, inW, _ := os.Pipe()
	realOut, realErr, realIn := os.Stdout, os.Stderr, os.Stdin
	os.Stdout, os.Stderr, os.Stdin = outW, errW, inR
	go func() {
		_, _ = inW.WriteString(stdin)
		inW.Close()
	}()
	// Both streams drain while the command runs: a pipe buffer is small (4KiB
	// on Windows), and a command that writes past it would block forever.
	outC, errC := make(chan []byte, 1), make(chan []byte, 1)
	go func() { b, _ := io.ReadAll(outR); outC <- b }()
	go func() { b, _ := io.ReadAll(errR); errC <- b }()

	code = run()

	os.Stdout, os.Stderr, os.Stdin = realOut, realErr, realIn
	outW.Close()
	errW.Close()
	return string(<-outC), string(<-errC), code
}

// askFlags is a request as the flags spell it, with everything a request
// must name.
var askFlags = []string{"request", "--name", "github", "--env-var", "GITHUB_TOKEN", "--hosts", "api.github.com"}

func TestListJSONIsTheProtocolList(t *testing.T) {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	credentials := []agentcreds.Credential{
		{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"}, Purpose: agentcreds.PurposeUse, ExpiresAt: &expires},
		{Name: "github", Hosts: []string{"github.com"}, Purpose: agentcreds.PurposeDelegate},
	}
	serve(t, &fakeService{credentials: credentials})

	stdout, _, code := capture(t, "", func() int { return Run([]string{"list", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got agentcreds.ListResponse
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON (%v): %s", err, stdout)
	}
	if len(got.Credentials) != 2 || got.Credentials[0].EnvVar != "GITHUB_TOKEN" || got.Credentials[1].Purpose != agentcreds.PurposeDelegate {
		t.Fatalf("list = %#v, want the service's credentials", got)
	}
}

// The text list says where each credential is and how long it lasts, and a
// delegation says it is delivered nowhere.
func TestListSaysWhereEachCredentialIs(t *testing.T) {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	serve(t, &fakeService{credentials: []agentcreds.Credential{
		{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com", "github.com"}, ExpiresAt: &expires},
		{Name: "npm", Hosts: []string{"registry.npmjs.org"}, Purpose: agentcreds.PurposeDelegate},
	}})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"github (GITHUB_TOKEN → api.github.com, github.com) [expires 2026-11-01T00:00:00Z]",
		"npm (delegate: give it to the discoboxes you create → registry.npmjs.org) [never expires]",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestListWithNothingSaysHowToAsk(t *testing.T) {
	serve(t, &fakeService{})
	stdout, _, code := capture(t, "", func() int { return Run([]string{"list"}) })
	if code != exitOK || !strings.Contains(stdout, "No credentials are granted") || !strings.Contains(stdout, "request --name NAME --env-var VAR --route") {
		t.Fatalf("exit = %d, stdout %q", code, stdout)
	}
}

func TestRequestJSONPreservesShellHostileText(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	justification := `the user's task says "open a PR" & $(do not expand) 'this'`
	body, err := json.Marshal(requestInput{
		Name:          "github",
		EnvVar:        "GITHUB_TOKEN",
		Hosts:         []string{"api.github.com"},
		Justification: justification,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	stdout, _, code := capture(t, string(body), func() int { return Run([]string{"request", "--json"}) })
	if code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stdout)
	}
	if svc.gotRequest.Justification != justification {
		t.Fatalf("justification = %q, want it unchanged", svc.gotRequest.Justification)
	}
}

// The flags carry the same ask the JSON body does.
func TestRequestFlagsCarryTheAsk(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	stdout, stderr, code := capture(t, "", func() int {
		return Run(append(askFlags, "--justification", "open a PR into main of acme/web", "--ttl", "4h"))
	})
	if code != exitOK || !strings.HasPrefix(stdout, "sreq_1 pending") {
		t.Fatalf("exit = %d, stdout %q, stderr %s", code, stdout, stderr)
	}
	want := agentcreds.RequestBody{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"},
		Justification: "open a PR into main of acme/web", GrantTTLSeconds: 4 * 3600}
	if fmt.Sprint(svc.gotRequest) != fmt.Sprint(want) {
		t.Fatalf("request = %#v, want %#v", svc.gotRequest, want)
	}
}

// A request names what it needs, where it goes, and the variable to put it
// in; one missing any is refused before anything is sent.
func TestRequestRefusesAnIncompleteAsk(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	for _, args := range [][]string{
		{"request", "--env-var", "GITHUB_TOKEN", "--hosts", "api.github.com"},
		{"request", "--name", "github", "--hosts", "api.github.com"},
		{"request", "--name", "github", "--env-var", "GITHUB_TOKEN"},
		// A positional is what a well-known ID used to be.
		{"request", "com.github.api", "--name", "github", "--env-var", "GITHUB_TOKEN", "--hosts", "api.github.com"},
	} {
		if _, _, code := capture(t, "", func() int { return Run(args) }); code != exitUsage {
			t.Fatalf("%q: exit = %d, want a usage error", args, code)
		}
	}
	if svc.gotRequest.Name != "" {
		t.Fatalf("request = %#v, want nothing sent", svc.gotRequest)
	}
}

// The lifetime an agent asks for reaches the service from either form of the
// command. One the flag cannot say in whole seconds is refused rather than
// truncated, since zero seconds would go out as no ask at all.
func TestRequestCarriesTheLifetimeAskedFor(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	body := `{"name":"github","envVar":"GITHUB_TOKEN","hosts":["api.github.com"],"grantTTLSeconds":14400}`
	if _, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if svc.gotRequest.GrantTTLSeconds != 14400 {
		t.Fatalf("grantTTLSeconds = %d, want the 14400 the JSON asked for", svc.gotRequest.GrantTTLSeconds)
	}

	svc.gotRequest = agentcreds.RequestBody{}
	if _, stderr, code := capture(t, "", func() int { return Run(append(askFlags, "--ttl", "30m")) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if svc.gotRequest.GrantTTLSeconds != 1800 {
		t.Fatalf("grantTTLSeconds = %d, want the 30m --ttl asked for", svc.gotRequest.GrantTTLSeconds)
	}

	svc.gotRequest = agentcreds.RequestBody{}
	if _, _, code := capture(t, "", func() int { return Run(append(askFlags, "--ttl", "1500ms")) }); code != exitUsage {
		t.Fatalf("exit = %d, want a usage error for a lifetime in fractions of a second", code)
	}
	negative := `{"name":"github","envVar":"GITHUB_TOKEN","hosts":["api.github.com"],"grantTTLSeconds":-1}`
	if _, _, code := capture(t, negative, func() int { return Run([]string{"request", "--json"}) }); code != exitUsage {
		t.Fatalf("exit = %d, want a usage error for a negative lifetime", code)
	}
	// The ceiling matters more than the floor: an ask nobody bounded is how a
	// human ends up one keystroke from a credential that outlives the work, and
	// one big enough to overflow a duration reads as forever.
	tooLong := fmt.Sprintf(`{"name":"github","envVar":"GITHUB_TOKEN","hosts":["api.github.com"],"grantTTLSeconds":%d}`, int64(agentcreds.MaxGrantTTLSeconds)+1)
	if _, stderr, code := capture(t, tooLong, func() int { return Run([]string{"request", "--json"}) }); code != exitUsage {
		t.Fatalf("exit = %d, want a usage error for an ask over the ceiling: %s", code, stderr)
	}
	if _, _, code := capture(t, "", func() int { return Run(append(askFlags, "--ttl", "720h1s")) }); code != exitUsage {
		t.Fatalf("exit = %d, want --ttl over thirty days refused", code)
	}
	if svc.gotRequest.Name != "" {
		t.Fatalf("request = %#v, want nothing sent for a lifetime that was refused", svc.gotRequest)
	}
}

// An ask to delegate a credential reaches the service as the delegate part,
// from either form of the command, and needs no variable (ADR 26-11-02-022
// §1).
func TestRequestCarriesAnAskToDelegate(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	body := `{"name":"github","delegate":{"routes":["GET api.github.com/repos/acme/web/**"]}}`
	if _, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if svc.gotRequest.Uses() || svc.gotRequest.Delegate == nil || !slices.Equal(svc.gotRequest.Delegate.Routes, []string{"GET api.github.com/repos/acme/web/**"}) {
		t.Fatalf("request = %#v, want only a delegate part", svc.gotRequest)
	}

	svc.gotRequest = agentcreds.RequestBody{}
	delegateFlags := []string{"request", "--name", "github", "--delegate-route", "GET api.github.com/repos/acme/web/**"}
	if _, stderr, code := capture(t, "", func() int { return Run(delegateFlags) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if svc.gotRequest.Uses() || svc.gotRequest.Delegate == nil || len(svc.gotRequest.Delegate.Routes) != 1 {
		t.Fatalf("request = %#v, want --delegate-route to ask to delegate", svc.gotRequest)
	}
}

// One request asks to use a credential and to delegate it, each part with
// its own routes, so a person answers both on one screen.
func TestRequestAsksToUseAndDelegateAtOnce(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	args := append(slices.Clone(askFlags), "--delegate-route", "POST api.github.com/repos/acme/web/pulls")
	if _, stderr, code := capture(t, "", func() int { return Run(args) }); code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	got := svc.gotRequest
	if got.EnvVar != "GITHUB_TOKEN" || !slices.Equal(got.Hosts, []string{"api.github.com"}) || got.Delegate == nil || !slices.Equal(got.Delegate.Routes, []string{"POST api.github.com/repos/acme/web/pulls"}) {
		t.Fatalf("request = %#v, want both parts with their own routes", got)
	}
}

// --delegate and a purpose are gone: one is refused as an unknown flag, the
// other as an unknown field, so an agent following an old example learns at
// once rather than asking for the wrong thing.
func TestTheOldAskToDelegateIsRefused(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	if _, _, code := capture(t, "", func() int { return Run(append(slices.Clone(askFlags), "--delegate")) }); code != exitUsage {
		t.Fatalf("--delegate: exit = %d, want a usage error", code)
	}
	body := `{"name":"github","hosts":["github.com"],"purpose":"delegate"}`
	_, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	if code != exitUsage || !strings.Contains(stderr, "purpose") {
		t.Fatalf("purpose: exit = %d, stderr %s", code, stderr)
	}
	if svc.gotRequest.Name != "" {
		t.Fatalf("request = %#v, want nothing sent", svc.gotRequest)
	}
}

func compact(t *testing.T, document string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(document)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func writePolicy(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := dir + "/" + name
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const issuesPolicy = `{
  "version": 1,
  "description": "Open issues in one repository",
  "parameters": {"owner": {"type": "pathSegment"}, "repo": {"type": "pathSegment"}},
  "routes": [{"hosts": ["api.github.com"], "methods": ["POST"], "paths": ["/repos/${owner}/${repo}/issues"]}]
}
`

// A policy file is sent as it was read, beside the arguments that follow its
// flag: each --policy-arg belongs to the --policy or --delegate-policy before
// it (ADR 26-11-02-022 §2). The client resolves nothing.
func TestRequestCarriesPolicyFilesWithTheirOwnArguments(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	dir := t.TempDir()
	issues := writePolicy(t, dir, "issues.json", issuesPolicy)
	args := []string{"request", "--name", "github", "--env-var", "GH_TOKEN",
		"--policy", issues, "--policy-arg", "owner=acme", "--policy-arg", "repo=web",
		"--delegate-policy", issues, "--policy-arg", "owner=acme", "--policy-arg", "repo=vm",
		"--policy", issues, "--policy-arg", "owner=acme", "--policy-arg", "repo=docs"}
	if _, stderr, code := capture(t, "", func() int { return Run(args) }); code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	got := svc.gotRequest
	if len(got.Policies) != 2 || got.Delegate == nil || len(got.Delegate.Policies) != 1 {
		t.Fatalf("request = %#v, want two use policies and one delegate policy", got)
	}
	for i, want := range []string{"web", "docs"} {
		if p := got.Policies[i]; p.File != issues || p.Args["repo"] != want || p.Args["owner"] != "acme" {
			t.Fatalf("use policy %d = %#v, want repo %s", i, p, want)
		}
	}
	if p := got.Delegate.Policies[0]; p.Args["repo"] != "vm" || len(p.Args) != 2 {
		t.Fatalf("delegate policy = %#v, want only its own arguments", p)
	}
	// The wire carries the document as JSON, which encoding/json compacts;
	// keys, their order and any repeated one are kept as they were read.
	if string(got.Policies[0].Document) != compact(t, issuesPolicy) {
		t.Fatalf("document = %q, want the file's document unchanged", got.Policies[0].Document)
	}
}

// The JSON form names policies by file too, and the CLI reads them.
func TestRequestJSONReadsItsPolicyFiles(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	dir := t.TempDir()
	issues := writePolicy(t, dir, "issues.json", issuesPolicy)
	body := fmt.Sprintf(`{"name":"github","delegate":{"policies":[{"file":%q,"args":{"owner":"acme","repo":"web"}}]}}`, issues)
	if _, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	if got := svc.gotRequest.Delegate; got == nil || len(got.Policies) != 1 || string(got.Policies[0].Document) != compact(t, issuesPolicy) || got.Policies[0].Args["repo"] != "web" {
		t.Fatalf("request = %#v", svc.gotRequest)
	}
}

// What the CLI can see wrong with a policy is refused before anything is
// sent, naming the file or the flag that fixes it.
func TestRequestRefusesABadPolicy(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	dir := t.TempDir()
	issues := writePolicy(t, dir, "issues.json", issuesPolicy)
	big := writePolicy(t, dir, "big.json", `{"version":1,"description":"`+strings.Repeat("x", agentcreds.MaxPolicyBytes)+`"}`)
	twice := writePolicy(t, dir, "twice.json", `{"version":1,"routes":[{"hosts":["a.example"],"hosts":["b.example"]}]}`)
	list := writePolicy(t, dir, "list.json", `[{"version":1}]`)
	two := writePolicy(t, dir, "two.json", `{"version":1} {"version":1}`)
	base := []string{"request", "--name", "github", "--env-var", "GH_TOKEN"}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"too large":         {[]string{"--policy", big}, "big.json"},
		"repeated key":      {[]string{"--policy", twice}, `"hosts" is repeated`},
		"not an object":     {[]string{"--policy", list}, "not a JSON object"},
		"two values":        {[]string{"--policy", two}, "more than one JSON value"},
		"missing":           {[]string{"--policy", dir + "/missing.json"}, "missing.json"},
		"arg before policy": {[]string{"--policy-arg", "owner=acme", "--policy", issues}, "give it after one"},
		"repeated arg":      {[]string{"--policy", issues, "--policy-arg", "repo=a", "--policy-arg", "repo=b"}, "given twice"},
		"beside routes":     {[]string{"--policy", issues, "--route", "api.github.com"}, "leave out --route"},
		"delegate beside":   {[]string{"--hosts", "h", "--delegate-policy", issues, "--delegate-route", "api.github.com"}, "leave out --delegate-route"},
	} {
		svc.gotRequest = agentcreds.RequestBody{}
		_, stderr, code := capture(t, "", func() int { return Run(append(slices.Clone(base), tc.args...)) })
		if code != exitUsage || !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: exit = %d, stderr %q, want a usage error naming %q", name, code, stderr, tc.want)
		}
		if svc.gotRequest.Name != "" {
			t.Errorf("%s: request sent, want nothing", name)
		}
	}
	var many []string
	for range agentcreds.MaxRequestPolicies + 1 {
		many = append(many, "--policy", issues)
	}
	if _, stderr, code := capture(t, "", func() int { return Run(append(slices.Clone(base), many...)) }); code != exitUsage || !strings.Contains(stderr, "at most") {
		t.Errorf("too many policies: exit = %d, stderr %q", code, stderr)
	}
}

// A JSON body that repeats a key is refused rather than read as its last
// value, at any depth: a duplicated argument must never be reduced silently.
func TestRequestJSONRefusesARepeatedKey(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	for name, body := range map[string]string{
		"top level": `{"name":"github","name":"other","envVar":"T","hosts":["h"]}`,
		"argument":  `{"name":"github","delegate":{"policies":[{"file":"p.json","args":{"repo":"a","repo":"b"}}]}}`,
	} {
		_, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
		if code != exitUsage || !strings.Contains(stderr, "repeated") {
			t.Errorf("%s: exit = %d, stderr %s", name, code, stderr)
		}
	}
	if svc.gotRequest.Name != "" {
		t.Fatalf("request = %#v, want nothing sent", svc.gotRequest)
	}
}

// What a request no longer carries — uses, a policy, a well-known ID — is
// refused as any unknown field is, so an agent working from an old example
// learns at once rather than from a person asking what it wanted.
func TestRequestJSONRejectsUnknownFields(t *testing.T) {
	serve(t, &fakeService{})

	for field, body := range map[string]string{
		"reason":     `{"name":"github","envVar":"T","hosts":["h"],"reason":"oops"}`,
		"uses":       `{"name":"github","envVar":"T","hosts":["h"],"uses":[{"description":"Open a PR"}]}`,
		"statements": `{"name":"github","envVar":"T","hosts":["h"],"statements":["allow read on anything in anywhere"]}`,
		"id":         `{"id":"com.github.api"}`,
	} {
		_, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
		if code != exitUsage {
			t.Fatalf("%s: exit = %d, want a usage failure", field, code)
		}
		var envelope errorEnvelope
		if err := json.Unmarshal([]byte(stderr), &envelope); err != nil {
			t.Fatalf("%s: stderr is not a JSON envelope (%v): %s", field, err, stderr)
		}
		if envelope.Error.Code != agentcreds.CodeInvalid || !strings.Contains(envelope.Error.Message, field) {
			t.Fatalf("%s: error = %#v, want invalid naming the field", field, envelope.Error)
		}
	}
}

// A refusal from the service surfaces with the protocol's stable code, and
// its message without the code repeated as a prefix.
func TestDenialFromTheServiceIsReportedAsAStableCode(t *testing.T) {
	serve(t, &fakeService{statusErr: fmt.Errorf("%w: not this sandbox's request", agentcreds.ErrDenied)})

	_, stderr, code := capture(t, "", func() int {
		return Run([]string{"wait", "--json", "--timeout", "5s", "request", "sreq_9"})
	})
	if code != exitError {
		t.Fatalf("exit = %d, want 1", code)
	}
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(lastLine(stderr)), &envelope); err != nil {
		t.Fatalf("stderr does not end in a JSON envelope (%v): %s", err, stderr)
	}
	if envelope.Error.Code != agentcreds.CodeDenied {
		t.Fatalf("code = %q, want %q", envelope.Error.Code, agentcreds.CodeDenied)
	}
	if strings.HasPrefix(envelope.Error.Message, "denied: ") {
		t.Fatalf("message = %q, want the detail without the sentinel prefix", envelope.Error.Message)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// A granted request says what the agent now holds, as the approver chose it,
// and that it is in its variable from now on.
func TestWaitReportsWhatWasGranted(t *testing.T) {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	granted := &agentcreds.Credential{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"}, ExpiresAt: &expires}
	serve(t, &fakeService{requestPolls: []agentcreds.RequestStatus{
		{RequestID: "sreq_1", Status: agentcreds.StatusPending},
		{RequestID: "sreq_1", Status: agentcreds.StatusGranted, Use: &agentcreds.PartStatus{Status: agentcreds.StatusGranted, Credential: granted}},
	}})
	stdout, stderr, code := capture(t, "", func() int { return Run(append(askFlags, "--wait", "--timeout", "10s")) })
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, stderr)
	}
	for _, want := range []string{
		"sreq_1 granted",
		"use: granted",
		"github (GITHUB_TOKEN → api.github.com) [expires 2026-11-01T00:00:00Z]",
		"in GITHUB_TOKEN for every command you start from now on",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

// A request a person granted the value itself says so, and where the value
// is, rather than promising a placeholder (ADR 26-10-27-840 §2).
func TestWaitSaysAGrantIsTheValueItself(t *testing.T) {
	granted := &agentcreds.Credential{Name: "aws", EnvVar: "AWS_KEY_FILE", Hosts: []string{"amazonaws.com"}, Raw: agentcreds.RawFile}
	serve(t, &fakeService{requestPolls: []agentcreds.RequestStatus{
		{RequestID: "sreq_1", Status: agentcreds.StatusGranted, Use: &agentcreds.PartStatus{Status: agentcreds.StatusGranted, Credential: granted}},
	}})
	stdout, stderr, code := capture(t, "", func() int { return Run(append(askFlags, "--wait", "--timeout", "10s")) })
	if code != exitOK {
		t.Fatalf("exit = %d, stderr %s", code, stderr)
	}
	for _, want := range []string{"AWS_KEY_FILE, the path of a file holding the value", "a person handed over the value itself: AWS_KEY_FILE holds the path of a file with it"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

func TestWaitOnADeniedRequestExitsNonZero(t *testing.T) {
	serve(t, &fakeService{status: agentcreds.RequestStatus{RequestID: "sreq_1", Status: agentcreds.StatusDenied}})

	body := `{"name":"github","envVar":"GITHUB_TOKEN","hosts":["api.github.com"],"wait":true,"timeoutSeconds":5}`
	_, _, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	if code == exitOK {
		t.Fatal("a denied request exited 0")
	}
}

// A request with one part granted and the other denied delivers the granted
// one, says so, and still exits non-zero: zero means every part asked for
// was granted (ADR 26-11-02-022 §4).
func TestAPartlyDeniedRequestExitsNonZero(t *testing.T) {
	granted := &agentcreds.Credential{Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com"}}
	serve(t, &fakeService{status: agentcreds.RequestStatus{
		RequestID: "sreq_1", Status: agentcreds.StatusGranted,
		Use:      &agentcreds.PartStatus{Status: agentcreds.StatusGranted, Credential: granted},
		Delegate: &agentcreds.PartStatus{Status: agentcreds.StatusDenied},
	}})
	args := append(slices.Clone(askFlags), "--delegate-route", "api.github.com", "--wait", "--timeout", "5s")
	stdout, stderr, code := capture(t, "", func() int { return Run(args) })
	if code != exitError {
		t.Fatalf("exit = %d, want 1: %s", code, stderr)
	}
	for _, want := range []string{"use: granted", "delegate: denied", "in GH_TOKEN for every command you start"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}

// run and facts are gone with the judge (ADR 26-10-27-880): a granted
// credential is already in its variable, so there is nothing to wrap.
func TestUnknownCommandIsAUsageError(t *testing.T) {
	for _, command := range []string{"summon", "run", "facts"} {
		if _, _, code := capture(t, "", func() int { return Run([]string{command}) }); code != exitUsage {
			t.Fatalf("%s: exit = %d, want %d", command, code, exitUsage)
		}
	}
}

// A credential sent to several sites is one request: --hosts takes them
// comma-separated, repeated, or both, and --json takes them as a list
// (ADR 26-10-02-393).
func TestRequestNamesSeveralHosts(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	for _, args := range [][]string{
		{"--hosts", "api.github.com,githubcopilot.com"},
		{"--hosts", "api.github.com", "--hosts", "githubcopilot.com"},
		{"--hosts", " api.github.com, ", "--hosts", "githubcopilot.com"},
	} {
		svc.gotRequest = agentcreds.RequestBody{}
		full := append([]string{"request", "--name", "github", "--env-var", "GH_TOKEN"}, args...)
		if _, stderr, code := capture(t, "", func() int { return Run(full) }); code != exitOK {
			t.Fatalf("%q: exit = %d, want 0: %s", args, code, stderr)
		}
		if got := strings.Join(svc.gotRequest.Hosts, ","); got != "api.github.com,githubcopilot.com" {
			t.Fatalf("%q: hosts = %q, want both, in order", args, got)
		}
	}

	svc.gotRequest = agentcreds.RequestBody{}
	body := `{"name":"github","envVar":"GH_TOKEN","hosts":["api.github.com","githubcopilot.com"]}`
	if _, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if got := strings.Join(svc.gotRequest.Hosts, ","); got != "api.github.com,githubcopilot.com" {
		t.Fatalf("hosts = %q, want both as written", got)
	}
}

// A request asks for routes: --route takes one each, a comma being the
// route's own, and --json takes them as a list, each sent as written.
func TestRequestAsksForRoutes(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)

	routes := []string{"GET,HEAD api.github.com/repos/acme/web/**,/user", "api.github.com/graphql graphql=mutation:createPullRequest,addComment"}
	full := []string{"request", "--name", "github", "--env-var", "GH_TOKEN", "--route", routes[0], "--route", routes[1]}
	if _, stderr, code := capture(t, "", func() int { return Run(full) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if !slices.Equal(svc.gotRequest.Routes, routes) || svc.gotRequest.Hosts != nil {
		t.Fatalf("request = %#v, want the routes as written and no hosts", svc.gotRequest)
	}

	svc.gotRequest = agentcreds.RequestBody{}
	body := `{"name":"github","envVar":"GH_TOKEN","routes":["GET api.github.com/repos/acme/web/**"],"hosts":["uploads.github.com"]}`
	if _, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) }); code != exitOK {
		t.Fatalf("exit = %d, want 0: %s", code, stderr)
	}
	if !slices.Equal(svc.gotRequest.Routes, []string{"GET api.github.com/repos/acme/web/**"}) || !slices.Equal(svc.gotRequest.Hosts, []string{"uploads.github.com"}) {
		t.Fatalf("request = %#v, want routes and hosts both", svc.gotRequest)
	}

	// A route that does not parse is the service's to refuse — this client
	// has no parser of its own (ADR 0031) — and its refusal reaches the agent
	// whole, naming the route, with the code it branches on.
	bad := "api.github.com graphql"
	svc.requestErr = fmt.Errorf("%w: route %q: graphql=OPS needs a comma list", agentcreds.ErrInvalid, bad)
	body = `{"name":"github","envVar":"GH_TOKEN","routes":["` + bad + `"]}`
	_, stderr, code := capture(t, body, func() int { return Run([]string{"request", "--json"}) })
	if code != exitError || !strings.Contains(stderr, `"code":"invalid"`) || !strings.Contains(stderr, bad) {
		t.Fatalf("exit = %d, stderr %q; want the service's invalid, naming the route", code, stderr)
	}
	_, stderr, code = capture(t, "", func() int {
		return Run([]string{"request", "--name", "github", "--env-var", "GH_TOKEN", "--route", bad})
	})
	if code != exitError || !strings.Contains(stderr, bad) {
		t.Fatalf("exit = %d, stderr %q; want the service's refusal, naming the route", code, stderr)
	}
}

// env prints what a shell evaluates to take up credentials granted after it
// started: one export per variable, sorted, each value quoted so the shell
// reads none of it.
func TestEnvPrintsExportsAShellEvaluates(t *testing.T) {
	svc := &fakeService{environment: map[string]string{
		"GH_TOKEN":  "ghp_sentinel",
		"NPM_TOKEN": "it's $(odd)",
	}}
	serve(t, svc)
	stdout, stderr, code := capture(t, "", func() int { return Run([]string{"env"}) })
	if code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr)
	}
	want := "export GH_TOKEN='ghp_sentinel'\nexport NPM_TOKEN='it'\\''s $(odd)'\n"
	if stdout != want {
		t.Fatalf("env =\n%s\nwant\n%s", stdout, want)
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", stdout+`printf %s "$NPM_TOKEN"`).Output() //nolint:gosec // The test evaluates what env printed: that is what it checks.
	if err != nil || string(out) != "it's $(odd)" {
		t.Fatalf("a shell read the export as %q, %v; want the value unchanged", out, err)
	}
	stdout, _, code = capture(t, "", func() int { return Run([]string{"env", "--json"}) })
	var got agentcreds.EnvironmentResponse
	if code != exitOK || json.Unmarshal([]byte(stdout), &got) != nil || !reflect.DeepEqual(got.Variables, svc.environment) {
		t.Fatalf("env --json = %q (exit %d), want the variables", stdout, code)
	}
	if _, _, code := capture(t, "", func() int { return Run([]string{"env", "GH_TOKEN"}) }); code != exitUsage {
		t.Fatalf("exit = %d, want a usage error for an argument", code)
	}
}

// `request --wait` that runs out of time before the answer exits exitPending,
// never 0: a script that asks and then runs on `&&` must not go on unapproved.
// A request that never waited still exits 0 with the request pending.
func TestRequestWaitThatRunsOutIsPending(t *testing.T) {
	serve(t, &fakeService{status: agentcreds.RequestStatus{RequestID: "sreq_1", Status: agentcreds.StatusPending}})
	stdout, stderr, code := capture(t, "", func() int { return Run(append(askFlags, "--wait", "--timeout", "20ms")) })
	if code != exitPending || !strings.Contains(stdout, "sreq_1 pending") || !strings.Contains(stderr, "wait --json request 'sreq_1'") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d, the request pending and how to resume", code, stdout, stderr, exitPending)
	}
	if _, _, code := capture(t, "", func() int { return Run(askFlags) }); code != exitOK {
		t.Fatalf("request without --wait exit = %d, want 0", code)
	}
}

// The outage this wait is for: the request is created, then every poll fails.
// The request is still open, as its creation said, so `request --wait` ends
// pending with the resume command and exit 3, not as a failure.
func TestRequestWaitThroughAnOutageIsPending(t *testing.T) {
	serve(t, &fakeService{statusErr: errors.New("the relay dropped the call")})
	stdout, stderr, code := capture(t, "", func() int { return Run(append(askFlags, "--wait", "--timeout", "50ms")) })
	if code != exitPending || !strings.Contains(stdout, "sreq_1 pending") || !strings.Contains(stderr, "wait --json request 'sreq_1'") {
		t.Fatalf("exit %d, stdout %q, stderr %q; want exit %d, the request pending and how to resume", code, stdout, stderr, exitPending)
	}
}

// Documents each within MaxPolicyBytes may still together outgrow the body
// the service reads: refused here, naming the limit, and nothing is sent.
func TestRequestRefusesPoliciesThatOutgrowTheBody(t *testing.T) {
	svc := &fakeService{}
	serve(t, svc)
	dir := t.TempDir()
	doc := `{"version":1,"description":"` + strings.Repeat("x", agentcreds.MaxPolicyBytes-200) + `","routes":[{"hosts":["a.example"]}]}`
	args := []string{"request", "--name", "github", "--env-var", "GH_TOKEN"}
	for i := range 20 {
		args = append(args, "--policy", writePolicy(t, dir, fmt.Sprintf("p%d.json", i), doc))
	}
	_, stderr, code := capture(t, "", func() int { return Run(args) })
	if code != exitUsage || !strings.Contains(stderr, "MaxBodyBytes") {
		t.Fatalf("exit = %d, stderr %q; want a usage error naming MaxBodyBytes", code, stderr)
	}
	if svc.gotRequest.Name != "" {
		t.Fatal("request sent, want nothing")
	}
}
