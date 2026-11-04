package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdoptionHelpExplainsTheWholeFlow(t *testing.T) {
	for _, tc := range []struct {
		command string
		words   []string
	}{
		{"adopt", []string{"--discobox-api", "--reason", "F4", "Ctrl-C", "request stays", "current parent", "wider egress", "discobox adopt mybox", "discobox ls --parent me"}},
		{"release", []string{"clearing its parent", "creator, files and credentials", "keeps running", "only a child it leads", "person's approval", "discobox release mybox"}},
	} {
		help := rootHelp(t, tc.command)
		for _, word := range tc.words {
			if !strings.Contains(help, word) {
				t.Errorf("%s help missing %q:\n%s", tc.command, word, help)
			}
		}
	}
}

func TestAdoptWaitsForApprovalOrDenial(t *testing.T) {
	child := testSandboxID(t)
	for _, status := range []string{"approved", "denied"} {
		t.Run(status, func(t *testing.T) {
			reply := func(status string) string {
				return `{"id":"adopt_review","projectId":"project-1","requesterSandboxId":"lead","childSandboxId":"` + child + `","justification":"review the feature","status":"` + status + `","createdAt":"2026-11-04T00:00:00Z","updatedAt":"2026-11-04T00:00:00Z"}`
			}
			calls, out, err := runDriveCommand(t, []string{reply("pending"), reply(status)}, "adopt", child, "--reason", "review the feature")
			if status == "approved" {
				if err != nil || out != "Now leading "+child+"\n" {
					t.Fatalf("approval printed %q, err=%v", out, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "adopt_review was denied") || out != "" {
				t.Fatalf("denial printed %q, err=%v", out, err)
			}
			if len(calls) != 2 || calls[0].method != "POST" || calls[0].path != "/projects/project-1/adoption-requests" || calls[1].method != "GET" || calls[1].path != "/projects/project-1/adoption-requests/adopt_review" {
				t.Fatalf("requests=%+v", calls)
			}
			if calls[0].body["childSandboxId"] != child || calls[0].body["justification"] != "review the feature" {
				t.Fatalf("request=%+v", calls[0])
			}
		})
	}
}

func TestReleaseClearsParentAndReportsTheChild(t *testing.T) {
	child := testSandboxID(t)
	var method, path string
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	cmd := NewRootCommand()
	out := new(strings.Builder)
	cmd.SetOut(out)
	cmd.SetErr(out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "release", child})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/projects/project-1/sandboxes/"+child+"/release" {
		t.Fatalf("request=%s %s", method, path)
	}
	if out.String() != "Released "+child+"\n" {
		t.Fatalf("output=%q", out.String())
	}
}

func TestAdoptRequiresAReasonBeforeSendingARequest(t *testing.T) {
	calls, out, err := runDriveCommand(t, []string{`{}`}, "adopt", testSandboxID(t))
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "reason" not set`) || out != "" {
		t.Fatalf("output=%q err=%v", out, err)
	}
	if len(calls) != 0 {
		t.Fatalf("sent requests without a reason: %+v", calls)
	}
}
