package access

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

func TestResumeWait(t *testing.T) {
	for _, kind := range []string{"request", "trust"} {
		for _, terminal := range []string{agentcreds.StatusGranted, agentcreds.StatusDenied} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				const id = "opaque_existing_id"
				granted := &agentcreds.Credential{Name: "github", EnvVar: "GITHUB_TOKEN", Hosts: []string{"github.com"}}
				svc := &fakeService{
					requestPolls: []agentcreds.RequestStatus{
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: terminal, Credential: granted},
					},
					trustPolls: []agentcreds.TrustRequestStatus{
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: terminal, Host: "github.com:443"},
					},
				}
				serve(t, svc)
				stdout, stderr, code := capture(t, "", func() int { return Run([]string{"wait", "--json", "--timeout", "10s", kind, id}) })
				wantCode := exitOK
				if terminal == agentcreds.StatusDenied {
					wantCode = exitError
				}
				if code != wantCode {
					t.Fatalf("exit %d, want %d: %s", code, wantCode, stderr)
				}
				var result struct {
					RequestID  string                 `json:"requestId"`
					Status     string                 `json:"status"`
					Credential *agentcreds.Credential `json:"credential"`
					Host       string                 `json:"host"`
				}
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatal(err)
				}
				if result.RequestID != id || result.Status != terminal {
					t.Fatalf("result: %#v", result)
				}
				if kind == "request" && (result.Credential == nil || result.Credential.EnvVar != "GITHUB_TOKEN") {
					t.Fatalf("result: %#v, want the credential granted", result)
				}
				if kind == "trust" && result.Host != "github.com:443" {
					t.Fatalf("result: %#v, want the trust's host", result)
				}
				assertProgress(t, stderr, kind, id, true)
				ids := svc.requestIDs
				if kind == "trust" {
					ids = svc.trustIDs
				}
				if len(ids) != 3 {
					t.Fatalf("polls: %v", ids)
				}
				for _, got := range ids {
					if got != id {
						t.Fatalf("polled %q, want %q", got, id)
					}
				}
				if svc.gotRequest.Name != "" || svc.gotTrust.Host != "" {
					t.Fatal("resuming created a new request")
				}
			})
		}
	}
}

func assertProgress(t *testing.T, stderr, kind, id string, waiting bool) {
	t.Helper()
	var event struct {
		Progress progressBody `json:"progress"`
	}
	if err := json.Unmarshal([]byte(stderr), &event); err != nil {
		t.Fatalf("invalid progress %q: %v", stderr, err)
	}
	if event.Progress.Status != "pending" || event.Progress.Kind != kind || event.Progress.RequestID != id || event.Progress.Waiting != waiting || event.Progress.Message == "" {
		t.Fatalf("progress: %#v", event.Progress)
	}
}

// A wait that ends before the answer comes is not approval and not failure:
// the request is still open. It says so on stdout, says how to resume, and
// exits exitPending, so a script that runs on `&&` does not go on as if it
// had been approved.
func TestWaitTimeoutDoesNotReportApproval(t *testing.T) {
	for _, kind := range []string{"request", "trust"} {
		t.Run(kind, func(t *testing.T) {
			svc := &fakeService{
				status:     agentcreds.RequestStatus{RequestID: "existing", Status: agentcreds.StatusPending},
				trustPolls: []agentcreds.TrustRequestStatus{{RequestID: "existing", Status: agentcreds.StatusPending}},
			}
			serve(t, svc)
			stdout, stderr, code := capture(t, "", func() int { return Run([]string{"wait", "--json", "--timeout", "20ms", kind, "existing"}) })
			var result struct {
				RequestID string `json:"requestId"`
				Status    string `json:"status"`
			}
			if code != exitPending || json.Unmarshal([]byte(stdout), &result) != nil || result.Status != agentcreds.StatusPending || result.RequestID != "existing" {
				t.Fatalf("exit %d, stdout %q, stderr %s; want exit %d and the request pending", code, stdout, stderr, exitPending)
			}
			dec := json.NewDecoder(strings.NewReader(stderr))
			for _, waiting := range []bool{true, false} {
				var event struct {
					Progress progressBody `json:"progress"`
				}
				if err := dec.Decode(&event); err != nil {
					t.Fatalf("stderr %s: %v", stderr, err)
				}
				if event.Progress.RequestID != "existing" || event.Progress.Waiting != waiting || !strings.Contains(event.Progress.Message, "wait --json "+kind+" 'existing'") {
					t.Fatalf("progress %#v, want waiting=%v with the resume command", event.Progress, waiting)
				}
			}
		})
	}
}

func TestWaitRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{}, {"unknown", "id"}, {"request"}, {"request", ""}, {"request", "../other"}, {"request", "."}, {"trust", ".."}, {"trust", "id?other"}, {"--timeout", "0", "request", "id"}} {
		_, _, code := capture(t, "", func() int { return Run(append([]string{"wait"}, args...)) })
		if code != exitUsage {
			t.Fatalf("args %v: exit %d", args, code)
		}
	}
}

func TestPendingCreationIncludesResumeNotice(t *testing.T) {
	for _, kind := range []string{"request", "trust"} {
		t.Run(kind, func(t *testing.T) {
			svc := &fakeService{trustStatus: agentcreds.TrustRequestStatus{RequestID: "treq_1", Status: agentcreds.StatusPending}}
			serve(t, svc)
			body := `{"name":"example","envVar":"EXAMPLE_TOKEN","hosts":["example.test"],"justification":"read"}`
			id := "sreq_1"
			if kind == "trust" {
				body = `{"host":"example.test","justification":"read"}`
				id = "treq_1"
			}
			stdout, stderr, code := capture(t, body, func() int { return Run([]string{kind, "--json"}) })
			if code != exitOK || !json.Valid([]byte(stdout)) {
				t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
			}
			assertProgress(t, stderr, kind, id, false)
		})
	}
}
