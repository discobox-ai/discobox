package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

// fakeControlPlane answers list and judge-commands the way the real control
// plane does, and records what it was asked — which is the half of
// ADR 26-09-22-838 §3 this package owns: that a command is put to the judge,
// and that nothing is minted unless the judge allowed it.
type fakeControlPlane struct {
	mu          sync.Mutex
	credentials []credentialDoc
	// judged is every command ask, and answer is how each is answered: a
	// judge answer, or a status with a problem document.
	judged   []commandAskDoc
	answer   judgeAnswer
	status   int
	problem  string
	requests []createCredentialRequestDoc
}

func newFakeControlPlane(t *testing.T, credentials []credentialDoc) (*controlPlaneCredentials, *fakeControlPlane) {
	t.Helper()
	root := withTestRoot(t)
	allow := true
	fake := &fakeControlPlane{credentials: credentials, answer: judgeAnswer{Allow: &allow, Reason: "matches the approved use"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandbox-credentials"):
			_ = json.NewEncoder(w).Encode(listCredentialsDoc{Credentials: fake.credentials})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandbox-credential-requests"):
			fake.mu.Lock()
			defer fake.mu.Unlock()
			var body createCredentialRequestDoc
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.requests = append(fake.requests, body)
			_ = json.NewEncoder(w).Encode(credentialRequestStatusDoc{RequestID: "req-1", Status: agentcreds.StatusPending})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/judge-commands"):
			fake.mu.Lock()
			defer fake.mu.Unlock()
			var body commandAskDoc
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.judged = append(fake.judged, body)
			if fake.status != 0 {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(fake.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"type": fake.problem, "detail": "said by the control plane"})
				return
			}
			_ = json.NewEncoder(w).Encode(fake.answer)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if err := WriteResolveContext(root, testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}
	broker := &controlPlaneCredentials{
		contextPath: root.ProxyResolveContextFile(testProjectID, testPoolID),
		client:      server.Client(),
	}
	return broker, fake
}

func oneUse() []credentialDoc {
	return []credentialDoc{{
		EnvVar: "GITHUB_TOKEN", Hosts: []string{"api.github.com"}, Sentinel: "STABLE-1",
		Uses: []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}},
	}}
}

func judgedBroker(plane *controlPlaneCredentials, live *activations) *credentialBroker {
	return &credentialBroker{sandboxID: "sb-1", controlPlan: plane, judge: plane, activations: live}
}

// A value is minted only once the project's judge allowed the command, and
// the judge is asked about the command as the sandbox declared it: its argv,
// what it reads on stdin, and where it says it runs.
func TestGetMintsOnlyWhatTheJudgeAllowed(t *testing.T) {
	plane, fake := newFakeControlPlane(t, oneUse())
	live := newActivations()

	out, err := judgedBroker(plane, live).Get(context.Background(), agentcreds.UseBody{
		UseID:    "use-1",
		Command:  []string{"gh", "pr", "create", "--body-file", "-"},
		Stdin:    &agentcreds.Stdin{Content: "Fixes #1"},
		Reported: &agentcreds.Reported{WorkingDirectory: "/src/repo"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.EnvVar != "GITHUB_TOKEN" || out.Value == "" {
		t.Fatalf("response = %#v, want a minted value", out)
	}
	if len(fake.judged) != 1 {
		t.Fatalf("judged %d commands, want exactly one", len(fake.judged))
	}
	asked := fake.judged[0]
	if asked.SandboxID != "sb-1" || asked.UseID != "use-1" || len(asked.Command) != 5 ||
		asked.Stdin == nil || asked.Stdin.Content != "Fixes #1" || asked.Reported == nil || asked.Reported.WorkingDirectory != "/src/repo" {
		t.Fatalf("asked = %#v, want this sandbox's use and the command as declared", asked)
	}
}

// A refusal mints nothing, and the judge's sentence is what the sandbox is
// told.
func TestGetMintsNothingTheJudgeRefused(t *testing.T) {
	plane, fake := newFakeControlPlane(t, oneUse())
	refuse := false
	fake.answer = judgeAnswer{Allow: &refuse, Reason: "deleting the repository is not opening a PR"}
	live := newActivations()

	_, err := judgedBroker(plane, live).Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh", "repo", "delete"}})
	if !errors.Is(err, agentcreds.ErrDenied) || !strings.Contains(err.Error(), "deleting the repository") {
		t.Fatalf("get error = %v, want ErrDenied carrying the judge's reason", err)
	}
	if len(live.byEphemeral) != 0 {
		t.Fatal("a refused command left an activation behind")
	}
}

// Anything that is not an allow refuses: a judge that could not be reached,
// or one that answered neither way. A command is asked about before anything
// exists to lose, so there is no reason to let one through on a silence.
func TestGetMintsNothingWithoutAnAnswer(t *testing.T) {
	for name, set := range map[string]func(*fakeControlPlane){
		"unreachable": func(f *fakeControlPlane) { f.status, f.problem = http.StatusServiceUnavailable, "about:blank" },
		"no decision": func(f *fakeControlPlane) { f.answer = judgeAnswer{Reason: "?"} },
	} {
		t.Run(name, func(t *testing.T) {
			plane, fake := newFakeControlPlane(t, oneUse())
			set(fake)
			live := newActivations()
			_, err := judgedBroker(plane, live).Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh"}})
			if !errors.Is(err, agentcreds.ErrDenied) || len(live.byEphemeral) != 0 {
				t.Fatalf("get error = %v with %d activations, want ErrDenied and none", err, len(live.byEphemeral))
			}
		})
	}
}

// A server that does not judge commands says so, and the value is minted
// without a verdict (ADR 26-10-02-054 §3).
func TestGetMintsUnjudgedOnAServerThatDoesNotJudgeCommands(t *testing.T) {
	plane, fake := newFakeControlPlane(t, oneUse())
	fake.status, fake.problem = http.StatusServiceUnavailable, judgingDisabledKind

	out, err := judgedBroker(plane, newActivations()).Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh"}})
	if err != nil || out.Value == "" {
		t.Fatalf("get = %#v, %v; want a value from a server that does not judge commands", out, err)
	}
}

// A value is only ever handed out for a command to judge, and a use nobody
// holds is refused before anybody is asked.
func TestGetAsksNothingForNoCommandOrNoUse(t *testing.T) {
	plane, fake := newFakeControlPlane(t, oneUse())
	b := judgedBroker(plane, newActivations())

	if _, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1"}); !errors.Is(err, agentcreds.ErrInvalid) {
		t.Fatalf("get error = %v, want ErrInvalid for no command", err)
	}
	if _, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-gone", Command: []string{"gh"}}); !errors.Is(err, agentcreds.ErrDenied) {
		t.Fatalf("get error = %v, want ErrDenied for a use nobody holds", err)
	}
	if len(fake.judged) != 0 {
		t.Fatalf("judged %d commands, want none", len(fake.judged))
	}
}

// A request by well-known ID goes out carrying the name, variable, and host the
// ID names, beside the ID the control plane checks them against; an ID the
// registry does not know is refused before anything is asked.
// The broker passes a well-known ask on as the agent sent it: the control
// plane fills in what the ID names and refuses what contradicts it, which it
// can only do if what the agent said reaches it unaltered.
func TestRequestPassesAWellKnownIDOnAsSent(t *testing.T) {
	broker, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: broker, activations: newActivations()}

	if _, err := b.Request(context.Background(), agentcreds.RequestBody{ID: "com.github.api", EnvVar: "GITHUB_TOKEN", Uses: []agentcreds.RequestedUse{{Description: "open a PR"}}}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(fake.requests) != 1 {
		t.Fatalf("requests = %d, want one", len(fake.requests))
	}
	got := fake.requests[0]
	if got.ID != "com.github.api" || got.Name != "" || got.EnvVar != "GITHUB_TOKEN" || got.Hosts != nil {
		t.Fatalf("request = %+v, want the ask as the agent sent it", got)
	}

	_, err := b.Request(context.Background(), agentcreds.RequestBody{ID: "com.example.nothing", Uses: []agentcreds.RequestedUse{{Description: "x"}}})
	if !errors.Is(err, agentcreds.ErrInvalid) || len(fake.requests) != 1 {
		t.Fatalf("err = %v, requests = %d; want an unknown ID refused as invalid before it is sent", err, len(fake.requests))
	}
}

// A use granted for several hosts is spent at any of them, and nowhere else,
// and list reports them all (ADR 26-10-02-393 §2).
func TestAUseGrantedForSeveralHostsIsSpentAtEach(t *testing.T) {
	broker, _ := newFakeControlPlane(t, []credentialDoc{{
		Name: "github", EnvVar: "GH_TOKEN", Sentinel: "STABLE-1",
		Hosts: []string{"api.github.com", "api.githubcopilot.com"},
		Uses:  []credentialUseDoc{{UseID: "use-1", Description: "run copilot"}},
	}})
	live := newActivations()
	b := judgedBroker(broker, live)

	listed, err := b.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || strings.Join(listed[0].Hosts, ",") != "api.github.com,api.githubcopilot.com" {
		t.Fatalf("listed = %+v, want both hosts", listed)
	}

	out, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"copilot"}})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resolver := &secretResolver{activations: live}
	for host, want := range map[string]bool{
		"api.github.com":        true,
		"api.githubcopilot.com": true,
		// A sibling of a granted host is not beneath it.
		"api.individual.githubcopilot.com": false,
		"github.com":                       false,
		"evil.example.com":                 false,
	} {
		if _, ok := resolver.activation(proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: out.Value, Host: host}); ok != want {
			t.Errorf("activation at %s = %v, want %v", host, ok, want)
		}
	}
}

// An activation is pinned to the hosts its use was granted for. One minted
// from a credential that names no host covers nothing — never the wildcard an
// empty scope would otherwise read as.
func TestAnActivationIsNeverPinnedToNoHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  credentialDoc
		at   map[string]bool
	}{
		{"one host", credentialDoc{Hosts: []string{"api.github.com"}}, map[string]bool{"api.github.com": true, "api.githubcopilot.com": false}},
		{"no host", credentialDoc{}, map[string]bool{"api.github.com": false, "evil.example.com": false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.doc
			doc.EnvVar, doc.Sentinel = "GH_TOKEN", "STABLE-1"
			doc.Uses = []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}}
			broker, _ := newFakeControlPlane(t, []credentialDoc{doc})
			live := newActivations()
			b := judgedBroker(broker, live)
			out, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh"}})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			resolver := &secretResolver{activations: live}
			for host, want := range tc.at {
				if _, ok := resolver.activation(proxy.SecretResolveRequest{ClientID: "sb-1", Sentinel: out.Value, Host: host}); ok != want {
					t.Errorf("activation at %s = %v, want %v", host, ok, want)
				}
			}
		})
	}
}

// An ask's hosts reach the control plane as the agent sent them.
func TestRequestPassesEveryHostOn(t *testing.T) {
	broker, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: broker, activations: newActivations()}
	if _, err := b.Request(context.Background(), agentcreds.RequestBody{
		Name: "github", EnvVar: "GH_TOKEN", Hosts: []string{"api.github.com", "api.githubcopilot.com"},
		Uses: []agentcreds.RequestedUse{{Description: "run copilot"}},
	}); err != nil {
		t.Fatalf("request: %v", err)
	}
	if len(fake.requests) != 1 || strings.Join(fake.requests[0].Hosts, ",") != "api.github.com,api.githubcopilot.com" {
		t.Fatalf("relayed = %+v, want the hosts as sent", fake.requests)
	}
}

// A granted request's lifetime is the approver's, and the sandbox polling it
// is told it on every granted use; a grant that never lapses carries none.
func TestRequestStatusCarriesTheGrantedLifetime(t *testing.T) {
	expires := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	uses := []credentialUseDoc{{UseID: "use_1", Description: "Push the branch"}}

	timed := credentialRequestStatusDoc{RequestID: "sreq_1", Status: agentcreds.StatusGranted, Uses: uses, ExpiresAt: &expires}.protocol()
	if got := timed.Uses[0].ExpiresAt; got == nil || !got.Equal(expires) {
		t.Fatalf("expiresAt = %v, want %v", got, expires)
	}
	forever := credentialRequestStatusDoc{RequestID: "sreq_1", Status: agentcreds.StatusGranted, Uses: uses}.protocol()
	if got := forever.Uses[0].ExpiresAt; got != nil {
		t.Fatalf("expiresAt = %v on a grant that never expires, want none", got)
	}
}
