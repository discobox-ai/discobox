package proxyagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/layout"
)

// fakeControlPlane answers list and sandbox-credential-verdicts the way the
// real control plane does, and records what it was sent — which is the half
// of ADR 0091's guarantee this package owns: that a verdict is sent, and sent
// before a value is ever minted.
type fakeControlPlane struct {
	mu           sync.Mutex
	credentials  []credentialDoc
	verdictCalls []recordCredentialVerdictDoc
	verdictErr   error
}

func newFakeControlPlane(t *testing.T, credentials []credentialDoc) (*controlPlaneCredentials, *fakeControlPlane) {
	t.Helper()
	withTestRoot(t)
	fake := &fakeControlPlane{credentials: credentials}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandbox-credentials"):
			_ = json.NewEncoder(w).Encode(listCredentialsDoc{Credentials: fake.credentials})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandbox-credential-verdicts"):
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.verdictErr != nil {
				http.Error(w, fake.verdictErr.Error(), http.StatusInternalServerError)
				return
			}
			var body recordCredentialVerdictDoc
			_ = json.NewDecoder(r.Body).Decode(&body)
			fake.verdictCalls = append(fake.verdictCalls, body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	if err := WriteResolveContext(testProjectID, testPoolID, server.URL, "tok"); err != nil {
		t.Fatalf("write resolve context: %v", err)
	}
	broker := &controlPlaneCredentials{
		contextPath: layout.ProxyResolveContextFile(testProjectID, testPoolID),
		client:      server.Client(),
	}
	return broker, fake
}

// The verdict must reach the control plane before the value does: ADR 0091's
// whole guarantee is that a credential is never issued without a record of
// why, and that only holds if the record actually lands.
func TestGetRecordsTheVerdictBeforeMintingTheValue(t *testing.T) {
	broker, fake := newFakeControlPlane(t, []credentialDoc{{
		EnvVar: "GITHUB_TOKEN", Host: "api.github.com", Sentinel: "STABLE-1",
		Uses: []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}},
	}})
	b := &credentialBroker{judge: allowPoolJudge, sandboxID: "sb-1", controlPlan: broker, activations: newActivations()}

	out, err := b.Get(context.Background(), agentcreds.UseBody{
		UseID:   "use-1",
		Command: []string{"gh", "pr", "create"},
	})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if out.EnvVar != "GITHUB_TOKEN" || out.Value == "" {
		t.Fatalf("response = %#v, want a minted value", out)
	}
	if len(fake.verdictCalls) != 1 {
		t.Fatalf("verdict calls = %d, want exactly one", len(fake.verdictCalls))
	}
	call := fake.verdictCalls[0]
	if call.SandboxID != "sb-1" || call.UseID != "use-1" || call.Volunteered {
		t.Fatalf("recorded verdict = %#v, want it scoped to this sandbox/use and not volunteered", call)
	}
	if !call.Verdict.Allow || call.Verdict.Role != "judge" {
		t.Fatalf("recorded verdict = %#v, want the allow verdict carried through", call.Verdict)
	}
}

func TestGetRefusesAMissingJudge(t *testing.T) {
	broker, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{sandboxID: "sb-1", controlPlan: broker, activations: newActivations()}
	_, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh"}})
	if err == nil {
		t.Fatal("issued a credential without a judge")
	}
	if len(fake.verdictCalls) != 0 {
		t.Fatal("attempted a verdict without a judge")
	}
}

// A control plane that cannot record the verdict must not mint a value
// anyway: the record is what makes the mint safe to have issued, not a
// courtesy alongside it.
func TestGetMintsNothingWhenRecordingTheVerdictFails(t *testing.T) {
	broker, fake := newFakeControlPlane(t, []credentialDoc{{
		EnvVar: "GITHUB_TOKEN", Host: "api.github.com", Sentinel: "STABLE-1",
		Uses: []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}},
	}})
	fake.verdictErr = errors.New("database unavailable")
	activations := newActivations()
	b := &credentialBroker{judge: allowPoolJudge, sandboxID: "sb-1", controlPlan: broker, activations: activations}

	_, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh", "pr", "create"}})
	if err == nil {
		t.Fatal("get minted a value even though its verdict could not be recorded")
	}
}

// A denial never reaches Get (ADR 0079 §1's ordering mints nothing for a
// refusal), so ReportDenial is the only route it reaches the control plane
// by, and it must reach it with Volunteered set.
func TestReportDenialRecordsAVolunteeredVerdict(t *testing.T) {
	broker, fake := newFakeControlPlane(t, nil)
	b := &credentialBroker{judge: allowPoolJudge, sandboxID: "sb-1", controlPlan: broker, activations: newActivations()}

	err := b.ReportDenial(context.Background(), agentcreds.DenialReport{
		UseID:   "use-1",
		Command: []string{"curl", "-X", "DELETE"},
		Verdict: agentcreds.Verdict{Allow: false, Reason: "broader than the approved use", Role: "judge", Prompt: "..."},
	})
	if err != nil {
		t.Fatalf("report denial: %v", err)
	}
	if len(fake.verdictCalls) != 1 || !fake.verdictCalls[0].Volunteered {
		t.Fatalf("verdict calls = %#v, want exactly one, volunteered", fake.verdictCalls)
	}
}

func allowPoolJudge(_ context.Context, _ judge.Job) (judge.Verdict, error) {
	return judge.Verdict{Allow: true, Reason: "matches the approved use", Role: judge.Role, Prompt: "...", PromptVersion: judge.PromptVersion, Revision: "rev-1", HarnessConfigID: "harness-1", Image: "sha256:test"}, nil
}

func stubPoolJudge(t *testing.T, fn func(judge.Job) (judge.Verdict, error)) {
	t.Helper()
	old := poolJudgeClient
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			return
		}
		var job judge.Job
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, "bad job", 400)
			return
		}
		v, err := fn(job)
		if err != nil {
			http.Error(w, "judge failed", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	poolJudgeClient = &http.Client{Transport: judgeTestTransport{base: server.Client().Transport, url: server.URL}}
	t.Cleanup(func() { poolJudgeClient = old; server.Close() })
}

type judgeTestTransport struct {
	base http.RoundTripper
	url  string
}

func (t judgeTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	target, _ := url.Parse(t.url)
	req.URL.Scheme = target.Scheme
	req.URL.Host = target.Host
	return t.base.RoundTrip(req)
}

func TestDeniedCommandIsRecordedWithoutMinting(t *testing.T) {
	broker, records := newFakeControlPlane(t, []credentialDoc{{Name: "github", Host: "github.com", Sentinel: "STABLE", Uses: []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}}}})
	live := newActivations()
	b := &credentialBroker{sandboxID: "sb", controlPlan: broker, activations: live, judge: func(_ context.Context, job judge.Job) (judge.Verdict, error) {
		if job.Purpose != "open a PR" || job.Command[0] != "delete" {
			t.Fatal("judge did not receive authoritative purpose and declared argv")
		}
		return judge.Verdict{Allow: false, Reason: "deletion is unrelated", Role: judge.Role}, nil
	}}
	if _, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"delete"}}); !errors.Is(err, agentcreds.ErrDenied) {
		t.Fatalf("error=%v", err)
	}
	if len(records.verdictCalls) != 1 || records.verdictCalls[0].Origin != "command" || records.verdictCalls[0].Verdict.Allow {
		t.Fatal("denial not recorded as a trusted command verdict")
	}
	if len(live.sentinelsByClient()) != 0 {
		t.Fatal("denied command minted a sentinel")
	}
}

func TestGrantRevokedDuringCommandJudgeDoesNotMint(t *testing.T) {
	broker, records := newFakeControlPlane(t, []credentialDoc{{Name: "github", Host: "github.com", Sentinel: "STABLE", Uses: []credentialUseDoc{{UseID: "use-1", Description: "open a PR"}}}})
	b := &credentialBroker{sandboxID: "sb", controlPlan: broker, activations: newActivations(), judge: func(ctx context.Context, job judge.Job) (judge.Verdict, error) {
		records.credentials = nil
		return allowPoolJudge(ctx, job)
	}}
	if _, err := b.Get(context.Background(), agentcreds.UseBody{UseID: "use-1", Command: []string{"gh"}}); !errors.Is(err, agentcreds.ErrDenied) {
		t.Fatalf("revoked use minted: %v", err)
	}
}

func TestJudgeUnavailableKeepsTheReasonWithoutAllowProvenance(t *testing.T) {
	stubPoolJudge(t, func(judge.Job) (judge.Verdict, error) {
		return judge.Verdict{Allow: false, Reason: "select a default or judge harness", Role: judge.Role, PromptVersion: judge.PromptVersion}, nil
	})
	verdict, err := callPoolJudge(t.Context(), judge.Job{Kind: "command", Purpose: "read status", Host: "github.com", Command: []string{"git", "status"}})
	if err != nil || verdict.Allow || verdict.Reason != "select a default or judge harness" {
		t.Fatalf("lost unavailable explanation: verdict=%+v err=%v", verdict, err)
	}
}
