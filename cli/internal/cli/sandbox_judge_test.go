package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// judgeListingServer answers the listing with the project's judge only when
// the caller asked for it, which is what the server does, and records every
// discobox the caller then went on to fetch.
func judgeListingServer(t *testing.T, fetched *[]string) *httptest.Server {
	t.Helper()
	const judgeID = "sbx_judge0000000001"
	const collection = "/projects/project-1/sandboxes"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == collection:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if r.URL.Query().Get("includeJudge") == "true" {
				_, _ = w.Write([]byte(`{"sandboxes":[` + namedSandboxJSON(judgeID, "judge-abc12345", "judge-abc12345", "present") + `]}`))
				return
			}
			_, _ = w.Write([]byte(`{"sandboxes":[]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, collection+"/"):
			*fetched = append(*fetched, strings.TrimPrefix(r.URL.Path, collection+"/"))
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(namedSandboxJSON(judgeID, "judge-abc12345", "judge-abc12345", "present")))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runJudgeCommand(t *testing.T, serverURL string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1"}, args...))
	err := cmd.Execute()
	return out.String() + errOut.String(), err
}

// A judge is left out of a listing because it is not what somebody asking what
// is in their project means; --include-judge is how they ask.
func TestBoxListLeavesTheJudgeOutUntilItIsAskedFor(t *testing.T) {
	var fetched []string
	server := judgeListingServer(t, &fetched)

	out, err := runJudgeCommand(t, server.URL, "admin", "box", "ls")
	if err != nil {
		t.Fatalf("box ls: %v", err)
	}
	if strings.Contains(out, "judge-abc12345") {
		t.Fatalf("box ls listed the judge:\n%s", out)
	}

	out, err = runJudgeCommand(t, server.URL, "admin", "box", "ls", "--include-judge")
	if err != nil {
		t.Fatalf("box ls --include-judge: %v", err)
	}
	if !strings.Contains(out, "judge-abc12345") {
		t.Fatalf("box ls --include-judge did not list the judge:\n%s", out)
	}
}

// Naming one is the other half: a short ID is resolved against a listing, and
// a listing that leaves the judge out would make the judge unnameable. This
// fails if the resolution stops asking for judges.
func TestAJudgeCanBeNamedByShortID(t *testing.T) {
	var fetched []string
	server := judgeListingServer(t, &fetched)

	if _, err := runJudgeCommand(t, server.URL, "admin", "box", "get", "sbx_judge0"); err != nil {
		t.Fatalf("box get by short ID: %v", err)
	}
	if len(fetched) != 1 || fetched[0] != "sbx_judge0000000001" {
		t.Fatalf("fetched = %v, want the short ID resolved to the judge's own ID", fetched)
	}
}
