package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
)

const (
	rmDocsID     = "sbx_9qk5n25t2hh2rv00"
	rmAPIID      = "sbx_2f7pn25t2hh2rv00"
	rmArchivedID = "sbx_4w8xn25t2hh2rv00"
)

// rmTestServer serves this directory's listing -- a titled discobox, an
// untitled one, and an archived one -- and records what was archived.
func rmTestServer(t *testing.T, archived *[]string) *httptest.Server {
	t.Helper()
	return rmTestServerListing(t, archived,
		namedSandboxJSON(rmDocsID, "docs", "Fixing the flaky test", "present")+`,`+
			namedSandboxJSON(rmAPIID, "api", "api", "present")+`,`+
			namedSandboxJSON(rmArchivedID, "stale", "stale", "archived"))
}

// rmTestServerListing is rmTestServer over a listing the test chose.
func rmTestServerListing(t *testing.T, archived *[]string, sandboxes string) *httptest.Server {
	t.Helper()
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		const collection = "/projects/project-1/sandboxes"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == collection:
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"sandboxes":[` + sandboxes + `]}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, collection+"/"):
			*archived = append(*archived, strings.TrimPrefix(r.URL.Path, collection+"/"))
			w.WriteHeader(http.StatusAccepted)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// namedSandboxJSON is one listing entry, wanted present or already archived.
// displayName is the NAME the table prints -- a terminal title, where the
// discobox has one -- and name is the configured name underneath it, the two
// being different for every discobox whose agent has titled its terminal.
func namedSandboxJSON(id, name, displayName, desiredState string) string {
	return `{"id":"` + id + `","projectId":"project-1","createdByUserId":"user-1","displayName":"` + displayName +
		`","config":{"name":"` + name + `","image":""},"runtime":{"state":"ready","runtimeState":"running","displayState":"running","desiredState":"` + desiredState + `","generation":1,"observedGeneration":1},` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
}

func runRM(t *testing.T, serverURL string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1"}, args...))
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

// One invocation archives every discobox it was given, and an argument is the
// NAME the listing prints, a full ID, or a short one.
func TestRemoveCommandArchivesEveryArgument(t *testing.T) {
	var archived []string
	server := rmTestServer(t, &archived)

	out, _, err := runRM(t, server.URL, "rm", "Fixing the flaky test", "sbx_2f7p", rmArchivedID)
	if err != nil {
		t.Fatalf("execute rm: %v", err)
	}
	if got, want := strings.Join(archived, ","), strings.Join([]string{rmDocsID, rmAPIID, rmArchivedID}, ","); got != want {
		t.Fatalf("archived = %q, want %q", got, want)
	}
	if got, want := out, rmDocsID+" archived\n"+rmAPIID+" archived\n"+rmArchivedID+" archived\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

// An already-archived discobox answers to its name. rm resolves against the
// whole `discobox ls` listing rather than the runtime candidates the pickers
// use, so a name the listing still shows never comes back as no such discobox;
// resolving through listProjectSandboxCandidates instead would fail this.
func TestRemoveCommandResolvesArchivedByName(t *testing.T) {
	var archived []string
	server := rmTestServer(t, &archived)

	if _, _, err := runRM(t, server.URL, "rm", "stale"); err != nil {
		t.Fatalf("execute rm of an archived discobox: %v", err)
	}
	if got, want := strings.Join(archived, ","), rmArchivedID; got != want {
		t.Fatalf("archived = %q, want %q", got, want)
	}
}

// delete is the same command: the API's own verb reaches it for anyone who
// read it there first.
func TestRemoveCommandDeleteAlias(t *testing.T) {
	var archived []string
	server := rmTestServer(t, &archived)

	if _, _, err := runRM(t, server.URL, "delete", "docs"); err != nil {
		t.Fatalf("execute delete: %v", err)
	}
	if got, want := strings.Join(archived, ","), rmDocsID; got != want {
		t.Fatalf("archived = %q, want %q", got, want)
	}
}

// An argument nothing in the directory answers to is reported the way it was
// written -- as a name, or, where its shape says it was meant as one, as an ID
// tried project-wide too -- and the arguments beside it still run.
func TestRemoveCommandReportsUnresolvedArgument(t *testing.T) {
	for _, tc := range []struct{ arg, want string }{
		{arg: "no-such-box", want: `no discobox named "no-such-box"`},
		// Shaped like a short ID, so it was tried as one as well.
		{arg: "nope", want: `no discobox for "nope"`},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			var archived []string
			server := rmTestServer(t, &archived)

			_, errOut, err := runRM(t, server.URL, "rm", tc.arg, "docs")
			if err == nil {
				t.Fatal("execute rm error = nil, want the failed argument reported")
			}
			if got, want := err.Error(), "failed to archive 1 discobox"; got != want {
				t.Fatalf("error = %q, want %q", got, want)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr = %q, want %q", errOut, tc.want)
			}
			// The rest of the arguments still ran.
			if got, want := strings.Join(archived, ","), rmDocsID; got != want {
				t.Fatalf("archived = %q, want %q", got, want)
			}
		})
	}
}

// The configured name still resolves for a discobox whose agent has titled its
// terminal, so a name learned before the title appeared does not stop working.
func TestRemoveCommandResolvesConfiguredNameBehindATitle(t *testing.T) {
	var archived []string
	server := rmTestServer(t, &archived)

	if _, _, err := runRM(t, server.URL, "rm", "docs"); err != nil {
		t.Fatalf("execute rm by configured name: %v", err)
	}
	if got, want := strings.Join(archived, ","), rmDocsID; got != want {
		t.Fatalf("archived = %q, want %q", got, want)
	}
}

// Two discoboxes under one window title is an everyday duplicate, and nothing
// is archived for an argument that cannot say which was meant.
func TestRemoveCommandRefusesAnAmbiguousName(t *testing.T) {
	var archived []string
	server := rmTestServerListing(t, &archived,
		namedSandboxJSON(rmDocsID, "docs", "Claude Code", "present")+`,`+
			namedSandboxJSON(rmAPIID, "api", "Claude Code", "present"))

	_, errOut, err := runRM(t, server.URL, "rm", "Claude Code")
	if err == nil {
		t.Fatal("execute rm error = nil, want the ambiguous argument refused")
	}
	if len(archived) != 0 {
		t.Fatalf("archived = %v, want nothing archived for an ambiguous name", archived)
	}
	for _, want := range []string{"names more than one discobox", rmDocsID, rmAPIID} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q, want it to contain %q", errOut, want)
		}
	}
}
