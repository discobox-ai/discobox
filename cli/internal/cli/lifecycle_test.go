package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/internal/hostid"
)

// lifecycleTestServer serves rmTestServer's listing and records each call it
// is sent: "list" for the listing, and "route sandboxID" for each start, stop,
// restart and suspend, answering with the sandbox and 202 Accepted the way the
// API does.
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
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, collection+"/"):
			// A start reads the discobox first, to deliver one still owed
			// its source in place of the start (deliverInsteadOfStart). It
			// is a read, not a call this records.
			sandboxID := strings.TrimPrefix(r.URL.Path, collection+"/")
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			_, _ = w.Write([]byte(namedSandboxJSON(sandboxID, "docs", "docs", "present")))
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
	for _, tc := range []struct{ verb, done, route string }{
		{"start", "started", "start"},
		{"stop", "stopped", "stop"},
		{"restart", "restarted", "restart"},
		// The CLI's word for the API's suspend.
		{"pause", "paused", "suspend"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			var posted []string
			server := lifecycleTestServer(t, &posted)

			out, errOut, err := runRM(t, server.URL, tc.verb, "Fixing the flaky test", "sbx_2f7p")
			if err != nil {
				t.Fatalf("execute %s: %v\n%s", tc.verb, err, errOut)
			}
			if got, want := strings.Join(posted, ","), "list,"+tc.route+" "+rmDocsID+","+tc.route+" "+rmAPIID; got != want {
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
// per argument and no listing: "start a discobox I created" says nothing of
// listing them. The hyphenated hostname spelling is a full ID too.
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

// A discobox running `discobox pause` is refused by the server (the sandbox
// role has no suspend), and the refusal is printed for each argument the way
// any failed power command's is.
func TestPauseReportsTheServersRefusal(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/suspend") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"status":403,"title":"Forbidden","detail":"sandbox role permission required for POST ` + r.URL.Path + `"}`))
	}))
	t.Cleanup(server.Close)

	_, errOut, err := runRM(t, server.URL, "pause", rmDocsID, rmAPIID)
	if err == nil {
		t.Fatal("execute pause error = nil, want the refusal reported")
	}
	if got, want := err.Error(), "failed to pause 2 discoboxes"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	for _, id := range []string{rmDocsID, rmAPIID} {
		if want := `failed to pause discobox "` + id + `": `; !strings.Contains(errOut, want) || !strings.Contains(errOut, "sandbox role permission required") {
			t.Fatalf("stderr = %q, want %q and the server's refusal", errOut, want)
		}
	}
}

// A box the API reports suspended reads paused in the table a person reads, and
// suspended in -o json, which prints the API's record (ADR 26-10-20-317 §4).
func TestPausedBoxListsAsPausedAndItsRecordSaysSuspended(t *testing.T) {
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	record := strings.ReplaceAll(namedSandboxJSON(rmDocsID, "docs", "docs", "present"), `"running"`, `"suspended"`)
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(record))
	}))
	t.Cleanup(server.Close)

	out, errOut, err := runRM(t, server.URL, "admin", "box", "get", rmDocsID)
	if err != nil {
		t.Fatalf("admin box get: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, " paused ") || strings.Contains(out, "suspended") {
		t.Fatalf("table = %q, want the state as paused", out)
	}
	out, errOut, err = runRM(t, server.URL, "-o", "json", "admin", "box", "get", rmDocsID)
	if err != nil {
		t.Fatalf("admin box get -o json: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, `"displayState": "suspended"`) || strings.Contains(out, "paused") {
		t.Fatalf("json = %q, want the API's suspended", out)
	}
}

// `admin box pause` is the raw form, and `admin box suspend`, the name it
// shipped under, still works without being taught: both post the API's
// suspend and print the record it answered with.
func TestAdminBoxPauseAndItsOldSpellingPostSuspend(t *testing.T) {
	for _, verb := range []string{"pause", "suspend"} {
		t.Run(verb, func(t *testing.T) {
			var posted []string
			server := lifecycleTestServer(t, &posted)
			out, errOut, err := runRM(t, server.URL, "admin", "box", verb, rmDocsID)
			if err != nil {
				t.Fatalf("admin box %s: %v\n%s", verb, err, errOut)
			}
			if got, want := strings.Join(posted, ","), "suspend "+rmDocsID; got != want {
				t.Fatalf("posted = %q, want %q", got, want)
			}
			if !strings.Contains(out, rmDocsID) {
				t.Fatalf("stdout = %q, want the record", out)
			}
		})
	}
	help, _, err := runRM(t, "http://127.0.0.1:1", "admin", "box", "--help")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help, "pause") || strings.Contains(help, "suspend") {
		t.Fatalf("admin box help = %q, want pause taught and suspend not", help)
	}
}

// The raw `admin box` forms take their DISCOBOX as every command acting on one
// does: the NAME `discobox ls` prints resolves to the ID the request carries.
func TestAdminBoxCommandsResolveADiscoboxByName(t *testing.T) {
	const id = "sbx_9qk5n25t2hh2rv00"
	for _, verb := range []string{"get", "start", "stop", "pause", "restart", "upgrade", "resources"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
			t.Chdir(t.TempDir())
			var asked []string
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				const collection = "/projects/project-1/sandboxes"
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				path := strings.TrimPrefix(r.URL.Path, "/api")
				switch {
				case r.Method == http.MethodGet && path == collection:
					_, _ = w.Write([]byte(`{"sandboxes":[` + namedSandboxJSON(id, "brave_aryabhata", "brave_aryabhata", "present") + `]}`))
				case strings.HasPrefix(path, collection+"/"):
					asked = append(asked, r.Method+" "+strings.TrimPrefix(path, collection+"/"))
					if r.Method == http.MethodPost {
						w.WriteHeader(http.StatusAccepted)
					}
					_, _ = w.Write([]byte(namedSandboxJSON(id, "brave_aryabhata", "brave_aryabhata", "present")))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)

			if _, _, err := runRM(t, server.URL, "-o", "json", "admin", "box", verb, "brave_aryabhata"); err != nil {
				t.Fatalf("admin box %s by name: %v", verb, err)
			}
			if len(asked) == 0 {
				t.Fatalf("admin box %s asked nothing of the discobox", verb)
			}
			for _, request := range asked {
				if path := strings.SplitN(request, " ", 2)[1]; path != id && !strings.HasPrefix(path, id+"/") {
					t.Fatalf("admin box %s asked %q, want discobox %s", verb, request, id)
				}
			}
		})
	}
}

// A DISCOBOX given blank names no discobox, so it is refused rather than taken
// as none given: the pick that follows needs no answer when one box is listed,
// and `admin box stop "$BOX"` with BOX unset would stop it.
func TestAdminCommandsRefuseABlankDiscobox(t *testing.T) {
	for _, args := range [][]string{
		{"admin", "box", "stop", ""},
		{"admin", "box", "transfer", " ", "--to", "lab"},
		{"admin", "harness", "discard", "", "candidate"},
	} {
		t.Run(args[2], func(t *testing.T) {
			t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
			t.Chdir(t.TempDir())
			server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				if r.Method == http.MethodGet && strings.TrimPrefix(r.URL.Path, "/api") == "/projects/project-1/sandboxes" {
					_, _ = w.Write([]byte(`{"sandboxes":[` + namedSandboxJSON("sbx_9qk5n25t2hh2rv00", "only", "only", "present") + `]}`))
					return
				}
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			t.Cleanup(server.Close)

			_, _, err := runRM(t, server.URL, args...)
			if err == nil || !strings.Contains(err.Error(), "DISCOBOX is blank") {
				t.Fatalf("%v: err = %v, want a blank DISCOBOX refused", args, err)
			}
		})
	}
}

// Under -o json a lifecycle command writes JSON naming each discobox it acted
// on, not the table form's "<id> stopped" lines a script cannot parse.
func TestLifecycleCommandsWriteJSONUnderOutputJSON(t *testing.T) {
	for _, verb := range []string{"start", "stop", "restart", "pause"} {
		t.Run(verb, func(t *testing.T) {
			var posted []string
			server := lifecycleTestServer(t, &posted)

			out, errOut, err := runRM(t, server.URL, "-o", "json", verb, rmDocsID)
			if err != nil {
				t.Fatalf("execute %s: %v\n%s", verb, err, errOut)
			}
			if !json.Valid([]byte(out)) {
				t.Fatalf("stdout under -o json is not JSON: %q", out)
			}
			if !strings.Contains(out, rmDocsID) {
				t.Fatalf("stdout = %q, want it to name %s", out, rmDocsID)
			}
		})
	}
}
