package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
)

// lifecycleTestServer serves rmTestServer's listing and records each call it
// is sent: "list" for the listing, and "verb sandboxID" for each start, stop,
// and restart, answering with the
// sandbox and 202 Accepted the way the API does.
func lifecycleTestServer(t *testing.T, posted *[]string) *httptest.Server {
	t.Helper()
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	listing := namedSandboxJSON(rmDocsID, "docs", "Fixing the flaky test", "present") + `,` +
		namedSandboxJSON(rmAPIID, "api", "api", "present")
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		const collection = "/projects/project-1/sandboxes"
		switch {
		case r.Method == http.MethodGet && r.URL.Path == collection:
			*posted = append(*posted, "list")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(`{"sandboxes":[` + listing + `]}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, collection+"/"):
			sandboxID, verb, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, collection+"/"), "/")
			*posted = append(*posted, verb+" "+sandboxID)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(namedSandboxJSON(sandboxID, "docs", "docs", "present")))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// Each root command posts its own call for every argument, resolving a NAME
// the listing prints, a full ID, or a short one.
func TestLifecycleCommandsPostEveryArgument(t *testing.T) {
	for _, tc := range []struct{ verb, done string }{
		{"start", "started"},
		{"stop", "stopped"},
		{"restart", "restarted"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			var posted []string
			server := lifecycleTestServer(t, &posted)

			out, errOut, err := runRM(t, server.URL, tc.verb, "Fixing the flaky test", "sbx_2f7p")
			if err != nil {
				t.Fatalf("execute %s: %v\n%s", tc.verb, err, errOut)
			}
			if got, want := strings.Join(posted, ","), "list,"+tc.verb+" "+rmDocsID+","+tc.verb+" "+rmAPIID; got != want {
				t.Fatalf("posted = %q, want %q", got, want)
			}
			if got, want := out, rmDocsID+" "+tc.done+"\n"+rmAPIID+" "+tc.done+"\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

// An argument that resolves to nothing is reported in the action's own words,
// and the arguments beside it still run.
func TestLifecycleCommandReportsUnresolvedArgument(t *testing.T) {
	var posted []string
	server := lifecycleTestServer(t, &posted)

	_, errOut, err := runRM(t, server.URL, "stop", "no-such-box", "api")
	if err == nil {
		t.Fatal("execute stop error = nil, want the failed argument reported")
	}
	if got, want := err.Error(), "failed to stop 1 discobox"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if want := `failed to stop discobox "no-such-box": no discobox named "no-such-box"`; !strings.Contains(errOut, want) {
		t.Fatalf("stderr = %q, want %q", errOut, want)
	}
	if got, want := strings.Join(posted, ","), "list,stop "+rmAPIID; got != want {
		t.Fatalf("posted = %q, want %q", got, want)
	}
}

// Failures are counted in English: two discoboxes, not two "discoboxs".
func TestLifecycleCommandCountsFailures(t *testing.T) {
	var posted []string
	server := lifecycleTestServer(t, &posted)

	_, _, err := runRM(t, server.URL, "restart", "no-such-box", "no-other-box")
	if err == nil {
		t.Fatal("execute restart error = nil, want both arguments reported")
	}
	if got, want := err.Error(), "failed to restart 2 discoboxes"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	if got, want := strings.Join(posted, ","), "list"; got != want {
		t.Fatalf("posted = %q, want %q", got, want)
	}
}

// Full IDs name their discoboxes outright, so the command makes the one call
// per argument and no listing: inside a discobox every call is judged against
// the use it was approved for, and "start a discobox I created" says nothing
// of listing them. The hyphenated hostname spelling is a full ID too.
func TestLifecycleCommandSkipsTheListingForFullIDs(t *testing.T) {
	var posted []string
	server := lifecycleTestServer(t, &posted)

	out, errOut, err := runRM(t, server.URL, "start", rmDocsID, strings.Replace(rmAPIID, "_", "-", 1))
	if err != nil {
		t.Fatalf("execute start: %v\n%s", err, errOut)
	}
	if got, want := strings.Join(posted, ","), "start "+rmDocsID+",start "+rmAPIID; got != want {
		t.Fatalf("posted = %q, want %q", got, want)
	}
	if got, want := out, rmDocsID+" started\n"+rmAPIID+" started\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
