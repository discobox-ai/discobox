package access

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/agentcreds"
)

func TestResumeWait(t *testing.T) {
	for _, kind := range []string{"request", "trust"} {
		for _, terminal := range []string{agentcreds.StatusGranted, agentcreds.StatusDenied} {
			t.Run(kind+"/"+terminal, func(t *testing.T) {
				const id = "opaque_existing_id"
				uses := []agentcreds.Use{{UseID: "use_ready", Description: "finish the task"}}
				svc := &fakeService{
					requestPolls: []agentcreds.RequestStatus{
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: terminal, Uses: uses},
					},
					trustPolls: []agentcreds.TrustRequestStatus{
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: agentcreds.StatusPending},
						{RequestID: id, Status: terminal, Uses: uses},
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
				var result agentcreds.RequestStatus
				if err := json.Unmarshal([]byte(stdout), &result); err != nil {
					t.Fatal(err)
				}
				if result.RequestID != id || result.Status != terminal || len(result.Uses) != 1 || result.Uses[0].UseID != "use_ready" {
					t.Fatalf("result: %#v", result)
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

func TestWaitTimeoutDoesNotReportApproval(t *testing.T) {
	for _, kind := range []string{"request", "trust"} {
		t.Run(kind, func(t *testing.T) {
			svc := &fakeService{status: agentcreds.RequestStatus{Status: agentcreds.StatusPending}, trustPolls: []agentcreds.TrustRequestStatus{{Status: agentcreds.StatusPending}}}
			serve(t, svc)
			stdout, stderr, code := capture(t, "", func() int { return Run([]string{"wait", "--json", "--timeout", "20ms", kind, "existing"}) })
			if code != exitError || stdout != "" {
				t.Fatalf("exit %d, stdout %q, stderr %s", code, stdout, stderr)
			}
			dec := json.NewDecoder(strings.NewReader(stderr))
			var progress struct {
				Progress progressBody `json:"progress"`
			}
			var failure errorEnvelope
			if err := dec.Decode(&progress); err != nil {
				t.Fatal(err)
			}
			if err := dec.Decode(&failure); err != nil {
				t.Fatal(err)
			}
			if progress.Progress.RequestID != "existing" || failure.Error.Code == "" {
				t.Fatalf("progress %#v, failure %#v", progress, failure)
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
			body := `{"name":"example","envVar":"EXAMPLE_TOKEN","host":"example.test","uses":[{"description":"read"}]}`
			id := "sreq_1"
			if kind == "trust" {
				body = `{"host":"example.test","uses":[{"description":"read"}]}`
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
