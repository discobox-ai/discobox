package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// fakeControlPlane answers the broker's routes the way the real control plane
// does, and records what it was asked.
type fakeControlPlane struct {
	mu          sync.Mutex
	credentials []credentialDoc
	requests    []createCredentialRequestDoc
	// status is what a request's poll answers with.
	status credentialRequestStatusDoc
	// listedFor is the sandbox each list named.
	listedFor []string
	// refuse, when set, is the status every call is refused with.
	refuse int
}

func newFakeControlPlane(t *testing.T, credentials []credentialDoc) (*controlPlaneCredentials, *fakeControlPlane) {
	t.Helper()
	root := withTestRoot(t)
	fake := &fakeControlPlane{credentials: credentials}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		if fake.refuse != 0 {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(fake.refuse)
			_ = json.NewEncoder(w).Encode(map[string]string{"detail": "said by the control plane"})
			return
		}
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandbox-credentials"):
			fake.listedFor = append(fake.listedFor, r.URL.Query().Get("sandboxId"))
			_ = json.NewEncoder(w).Encode(listCredentialsDoc{Credentials: fake.credentials})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandbox-credential-requests"):
			var body createCredentialRequestDoc
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.requests = append(fake.requests, body)
			_ = json.NewEncoder(w).Encode(credentialRequestStatusDoc{RequestID: "req-1", Status: agentcreds.StatusPending})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/sandbox-credential-requests/"):
			_ = json.NewEncoder(w).Encode(fake.status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if err := WriteResolveContext(root, testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}
	return &controlPlaneCredentials{
		contextPath: root.ProxyResolveContextFile(testProjectID, testPoolID),
		client:      server.Client(),
	}, fake
}

// List reports what the control plane says the sandbox the certificate named
// holds, and never anything that could stand for the credential.
func TestListReportsTheCredentialsOfTheCertificatesSandbox(t *testing.T) {
	expires := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	plane, fake := newFakeControlPlane(t, []credentialDoc{
		{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com", "githubcopilot.com"}, SecretID: "sec_1", GrantID: "sgr_1", ExpiresAt: &expires},
		{Name: "github", Hosts: []string{"github.com"}, SecretID: "sec_1", GrantID: "sgr_2", Purpose: agentcreds.PurposeDelegate},
	})
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: plane}
	got, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []agentcreds.Credential{
		{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com", "githubcopilot.com"}, ExpiresAt: &expires},
		{Name: "github", Hosts: []string{"github.com"}, Purpose: agentcreds.PurposeDelegate},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list = %+v, want %+v", got, want)
	}
	if len(fake.listedFor) != 1 || fake.listedFor[0] != "sb-1" {
		t.Fatalf("listed for %v, want the certificate's sandbox", fake.listedFor)
	}
}

// An ask reaches the control plane as the agent sent it, for the sandbox the
// certificate named.
func TestRequestPassesTheAskOn(t *testing.T) {
	plane, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: plane}
	status, err := b.Request(context.Background(), agentcreds.RequestBody{
		Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com", "api.githubcopilot.com"},
		Routes:        []string{"api.github.com/graphql graphql=query"},
		Justification: "run copilot", GrantTTLSeconds: 3600, Purpose: agentcreds.PurposeUse,
	})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if status.RequestID != "req-1" || status.Status != agentcreds.StatusPending || status.Credential != nil {
		t.Fatalf("status = %+v, want the pending request and nothing granted", status)
	}
	want := createCredentialRequestDoc{
		SandboxID: "sb-1", Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com", "api.githubcopilot.com"},
		Routes:        []string{"api.github.com/graphql graphql=query"},
		Justification: "run copilot", GrantTTLSeconds: 3600, Purpose: agentcreds.PurposeUse,
	}
	if len(fake.requests) != 1 || !reflect.DeepEqual(fake.requests[0], want) {
		t.Fatalf("relayed = %+v, want %+v", fake.requests, want)
	}
}

// A granted request says what the sandbox now holds, as the approver chose it;
// a request still pending or denied says nothing about a credential.
func TestRequestStatusSaysWhatWasGranted(t *testing.T) {
	expires := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	plane, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: plane}
	fake.status = credentialRequestStatusDoc{
		RequestID: "sreq_1", Status: agentcreds.StatusGranted, Purpose: agentcreds.PurposeUse, ExpiresAt: &expires,
		Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com"},
	}
	got, err := b.RequestStatus(context.Background(), "sreq_1")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	want := &agentcreds.Credential{Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com"}, Purpose: agentcreds.PurposeUse, ExpiresAt: &expires}
	if got.Status != agentcreds.StatusGranted || !reflect.DeepEqual(got.Credential, want) {
		t.Fatalf("status = %+v, want %+v granted", got, want)
	}
	fake.status = credentialRequestStatusDoc{RequestID: "sreq_1", Status: agentcreds.StatusDenied}
	if got, err := b.RequestStatus(context.Background(), "sreq_1"); err != nil || got.Credential != nil {
		t.Fatalf("denied status = %+v (%v), want no credential", got, err)
	}
}

// A credential the sandbox holds raw says so in the list (ADR 26-10-27-840),
// so an agent knows its variable holds the value, or a file's path.
func TestListSaysACredentialIsHeldRaw(t *testing.T) {
	plane, _ := newFakeControlPlane(t, []credentialDoc{
		{Name: "aws", EnvVar: "AWS_SECRET_ACCESS_KEY", Hosts: []string{"amazonaws.com"}, SecretID: "sec_1", Raw: agentcreds.RawFile},
	})
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: plane}
	listed, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || listed[0].Raw != agentcreds.RawFile {
		t.Fatalf("list = %+v, want the credential held raw in a file", listed)
	}
}

// A refusal reaches the sandbox as a refusal, in the control plane's words,
// rather than as an outage two hops away.
func TestAControlPlaneRefusalIsADenial(t *testing.T) {
	plane, fake := newFakeControlPlane(t, nil)
	fake.refuse = http.StatusForbidden
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: plane}
	_, err := b.RequestStatus(context.Background(), "sreq_other")
	if !errors.Is(err, agentcreds.ErrDenied) || !strings.Contains(err.Error(), "said by the control plane") {
		t.Fatalf("err = %v, want a denial carrying the control plane's sentence", err)
	}
}

// The environment read is the calling sandbox's credential variables and the
// sentinel delivered in each, as the pool agent last delivered them, and
// nobody else's.
func TestEnvironmentIsTheCertificatesSandboxsCredentialVariables(t *testing.T) {
	root := withTestRoot(t)
	if err := SetSandboxEnvironment(root, testProjectID, testPoolID, "sb-1", map[string]string{"GH_TOKEN": "ghp_SENTINEL"}); err != nil {
		t.Fatal(err)
	}
	if err := SetSandboxEnvironment(root, testProjectID, testPoolID, "sb-2", map[string]string{"NPM_TOKEN": "npm_SENTINEL"}); err != nil {
		t.Fatal(err)
	}
	path := root.ProxySecretsFile(testProjectID, testPoolID)
	got, err := (&credentialBroker{sandboxID: "sb-1", secretsPath: path}).Environment(context.Background())
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]string{"GH_TOKEN": "ghp_SENTINEL"}) {
		t.Fatalf("environment = %v, want sb-1's alone", got)
	}
	if got, err := (&credentialBroker{sandboxID: "sb-3", secretsPath: path}).Environment(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("a sandbox with no credentials read %v (%v), want none", got, err)
	}
	if err := RemoveSandboxPolicy(root, testProjectID, testPoolID, "sb-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := (&credentialBroker{sandboxID: "sb-1", secretsPath: path}).Environment(context.Background()); len(got) != 0 {
		t.Fatalf("environment = %v after the sandbox's policy was removed, want none", got)
	}
}
