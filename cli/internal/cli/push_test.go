package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const pushTestSandboxID = "sbx_9qk5n25t2hh2rv00"

// awaitingSourceServer answers with one discobox parked waiting for the source
// its create never delivered, and records every path the command asks for so a
// test can assert that nothing was pushed.
func awaitingSourceServer(t *testing.T, source map[string]any, paths *[]string) *httptest.Server {
	t.Helper()
	sandbox := map[string]any{
		"id":              pushTestSandboxID,
		"projectId":       "project-1",
		"createdByUserId": "user-1",
		"displayName":     "parked",
		"config":          map[string]any{"name": "parked", "image": "", "source": source},
		"runtime": map[string]any{
			"state":              "awaiting_source",
			"displayState":       "starting",
			"desiredState":       "present",
			"generation":         1,
			"observedGeneration": 1,
		},
		"createdAt": "2026-06-17T00:00:00Z",
		"updatedAt": "2026-06-17T00:00:01Z",
	}
	body, err := json.Marshal(sandbox)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		if !strings.HasSuffix(r.URL.Path, "/sandboxes/"+pushTestSandboxID) {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func pushTestSource(t *testing.T, mutate func(map[string]any)) map[string]any {
	t.Helper()
	source := map[string]any{
		"kind":     "git",
		"slug":     "primary",
		"delivery": "push",
		"checkout": map[string]any{
			"commit":  "0123456789abcdef0123456789abcdef01234567",
			"refName": "main",
			"refType": "branch",
		},
		"destination": map[string]any{"directory": "/workspace/source"},
	}
	if mutate != nil {
		mutate(source)
	}
	return source
}

func runPushCommand(t *testing.T, serverURL string, args ...string) error {
	t.Helper()
	cmd := NewRootCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1", "push", pushTestSandboxID}, args...))
	return cmd.Execute()
}

// A discobox still waiting for its source is delivered, not offered something
// to rebase onto, so the flags that shape a rebase-time push have nothing to
// act on. Each is refused by name rather than quietly ignored.
func TestPushRefusesRebaseFlagsWhileTheDiscoboxIsWaitingForItsSource(t *testing.T) {
	var paths []string
	server := awaitingSourceServer(t, pushTestSource(t, nil), &paths)

	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{name: "source", args: []string{"--source", "primary"}, want: "--source cannot narrow"},
		{name: "branch", args: []string{"--branch", "other"}, want: "--branch applies once it is running"},
		{name: "force", args: []string{"--force"}, want: "nothing to force past"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := runPushCommand(t, server.URL, tt.args...)
			if err == nil {
				t.Fatalf("push %v: got nil error, want a refusal", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "waiting for its source") {
				t.Fatalf("error = %v, want it to say the discobox is still waiting", err)
			}
		})
	}
}

// The delivery is resolved and checked before any of it is sent: a discobox
// resumes on one report covering every source, so a delivery that cannot finish
// must not leave half of itself in the origin.
func TestPushRefusesADeliveryThisMachineCanNoLongerFinish(t *testing.T) {
	repo := newRunSourceTestRepo(t)

	t.Run("commit is gone", func(t *testing.T) {
		var paths []string
		server := awaitingSourceServer(t, pushTestSource(t, nil), &paths)
		err := runPushCommand(t, server.URL, "--dir", "primary="+repo)
		if err == nil || !strings.Contains(err.Error(), "0123456789abcdef0123456789abcdef01234567") {
			t.Fatalf("error = %v, want it to name the commit that is not here", err)
		}
		assertNothingPushed(t, paths)
	})

	t.Run("the run's own repository is gone", func(t *testing.T) {
		var paths []string
		source := pushTestSource(t, func(s map[string]any) { s["noLocalRepository"] = true })
		server := awaitingSourceServer(t, source, &paths)
		err := runPushCommand(t, server.URL, "--dir", "primary="+repo)
		if err == nil || !strings.Contains(err.Error(), "not a Git repository") {
			t.Fatalf("error = %v, want it to say the run's repository is gone", err)
		}
		assertNothingPushed(t, paths)
	})
}

func assertNothingPushed(t *testing.T, paths []string) {
	t.Helper()
	for _, path := range paths {
		if strings.Contains(path, "git-origins") {
			t.Fatalf("requested %s, want nothing pushed before the refusal", path)
		}
	}
}
