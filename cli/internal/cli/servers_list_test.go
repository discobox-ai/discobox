package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/internal/hostid"
)

const (
	sandboxA = "sbx_9qk5n25t2hh2rv0a"
	sandboxB = "sbx_9qk5n25t2hh2rv0b"
	sandboxC = "sbx_9qk5n25t2hh2rv0c"
)

// listedSandboxJSON is one discobox the way a server lists it.
func listedSandboxJSON(id, created string) string {
	// Named after its ID's last character rather than after the ID itself: a
	// discobox whose name is its own ID claims the ID's own patterns twice, so
	// every one of them is dropped as ambiguous (sshConfigHostPatterns) and it
	// gets no ssh alias at all.
	name := "box-" + id[len(id)-1:]
	return fmt.Sprintf(`{"id":%q,"projectId":"project-1","createdByUserId":"user-1","displayName":%q,`+
		`"config":{"name":%q,"image":""},"runtime":{"state":"ready","runtimeState":"running","displayState":"running",`+
		`"desiredState":"present","generation":1,"observedGeneration":1},"createdAt":%q,"updatedAt":%q}`,
		id, name, name, created, created)
}

// fakeServerHandler answers what listing, finding and registering ask a
// server: what it is called, its projects, and its discoboxes.
func fakeServerHandler(name string, sandboxIDs ...string) http.Handler {
	return fakePeerServerHandler(name, "", sandboxIDs...)
}

// fakePeerServerHandler is fakeServerHandler for a server that listens for
// peers as peerID.
func fakePeerServerHandler(name, peerID string, sandboxIDs ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/server":
			_, _ = fmt.Fprintf(w, `{"name":%q}`, name)
		case r.Method == http.MethodGet && r.URL.Path == "/peer":
			if peerID == "" {
				_, _ = w.Write([]byte(`{}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"peerId":%q}`, peerID)
		case r.Method == http.MethodGet && r.URL.Path == "/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		case r.URL.Path == "/ssh":
			_, _ = w.Write([]byte(sshConfigEnabledIngress))
		case strings.HasSuffix(r.URL.Path, "/ssh-keys"):
			if r.Method == http.MethodPost {
				_, _ = fmt.Fprintf(w, `{"id":"sshkey_1","projectId":%q,"publicKey":"ssh-ed25519 AAAA","fingerprint":"SHA256:test","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`, fakeProjectID(name))
				return
			}
			_, _ = w.Write([]byte(`{"sshKeys":[]}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/projects/") && strings.Count(r.URL.Path, "/") == 2:
			// Whatever project was asked for, this server's own: the files a
			// sync writes are named by what the server says it is.
			_, _ = fmt.Fprintf(w, `{"id":%q,"name":"P","ownerUserId":"user-1","default":false,"welcomed":true,`+
				`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`, fakeProjectID(name))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/secret-requests"):
			// One request waiting on each discobox, named for the server, so a
			// test can tell whose inbox a request came from.
			rows := make([]string, 0, len(sandboxIDs))
			for _, id := range sandboxIDs {
				rows = append(rows, fmt.Sprintf(`{"id":"sreq_%s_%s","projectId":%q,"sandboxId":%q,"requestedBy":"agent","type":"token","status":"pending",`+
					`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`, name, id, fakeProjectID(name), id))
			}
			_, _ = fmt.Fprintf(w, `{"secretRequests":[%s]}`, strings.Join(rows, ","))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/projects/") && strings.HasSuffix(r.URL.Path, "/sandboxes"):
			rows := make([]string, 0, len(sandboxIDs))
			for i, id := range sandboxIDs {
				rows = append(rows, listedSandboxJSON(id, fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1)))
			}
			_, _ = fmt.Fprintf(w, `{"sandboxes":[%s]}`, strings.Join(rows, ","))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// fakeProjectID is the project one fake server reports, which is what names
// the files a config sync writes for it.
func fakeProjectID(server string) string {
	return "proj_" + sanitizeServerName(server)
}

func fakeServer(t *testing.T, name string, sandboxIDs ...string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fakeServerHandler(name, sandboxIDs...))
	t.Cleanup(server.Close)
	return server
}

// deadServer is an address nothing answers at.
func deadServer(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	return server.URL
}

func commandForTest() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	return cmd, &stderr
}

// A listing is every server's, each discobox named by the server it is on, and
// a server registered under a second address lists nothing twice.
func TestListEveryServerListsEachServerOnce(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "Alpha", sandboxA)
	other := fakeServer(t, "beta", sandboxB, sandboxA)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	listed, unreachable, err := app.listEveryServer(context.Background(), true, nil)
	if err != nil {
		t.Fatalf("listEveryServer() error = %v", err)
	}
	if len(unreachable) != 0 {
		t.Fatalf("unreachable = %+v, want none", unreachable)
	}
	got := map[string]string{}
	for _, row := range listed {
		got[row.sandbox.ID] = row.server.name
	}
	// The primary nobody registered goes by the name it offers.
	want := map[string]string{sandboxA: "Alpha", sandboxB: "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("listed = %v, want %v", got, want)
	}
}

// A registered server that does not answer costs the listing its own
// discoboxes and nothing else; the primary not answering fails it, as it
// always did.
func TestListEveryServerLeavesOutARegisteredServerThatDoesNotAnswer(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	registerForTest(t, registeredServer{Name: "gone", Address: deadServer(t)})

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	listed, unreachable, err := app.listEveryServer(context.Background(), true, nil)
	if err != nil {
		t.Fatalf("listEveryServer() error = %v", err)
	}
	if len(listed) != 1 || listed[0].sandbox.ID != sandboxA {
		t.Fatalf("listed = %+v, want the primary's discobox", listed)
	}
	if len(unreachable) != 1 || unreachable[0].server.name != "gone" {
		t.Fatalf("unreachable = %+v, want gone", unreachable)
	}

	app = &App{serverURL: deadServer(t), projectID: "project-1"}
	if _, _, err := app.listEveryServer(context.Background(), true, nil); err == nil {
		t.Fatal("listEveryServer() succeeded with the primary down")
	}
}

func TestLsNamesEachDiscoboxesServer(t *testing.T) {
	useTempServersFile(t)
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	other := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	run := func(args ...string) string {
		t.Helper()
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs(append([]string{"--server", primary.URL, "--project", "project-1"}, args...))
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}

	table := run("ls", "--all")
	header := strings.Fields(strings.SplitN(table, "\n", 2)[0])
	if len(header) < 3 || header[2] != "SERVER" {
		t.Fatalf("header = %v, want a SERVER column after NAME", header)
	}
	for _, want := range []string{sandboxA + "  ", "alpha", sandboxB, "beta"} {
		if !strings.Contains(table, want) {
			t.Fatalf("ls =\n%s\nwant it to carry %q", table, want)
		}
	}

	var body struct {
		Sandboxes []struct {
			Server string `json:"server"`
			ID     string `json:"id"`
		} `json:"sandboxes"`
	}
	if err := json.Unmarshal([]byte(run("ls", "--all", "-o", "json")), &body); err != nil {
		t.Fatalf("ls -o json: %v", err)
	}
	got := map[string]string{}
	for _, row := range body.Sandboxes {
		got[row.ID] = row.Server
	}
	if !reflect.DeepEqual(got, map[string]string{sandboxA: "alpha", sandboxB: "beta"}) {
		t.Fatalf("ls -o json servers = %v", got)
	}
}

// An ID copied from `discobox ls` works whichever server listed it: the
// primary is asked first, and a registered server only when the primary has no
// such discobox.
func TestSelectSandboxFindsAnIDOnWhicheverServerHasIt(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	other := fakeServer(t, "beta", sandboxB, sandboxA)
	third := fakeServer(t, "gamma", sandboxC)
	registerForTest(t,
		registeredServer{Name: "beta", Address: other.URL},
		registeredServer{Name: "gamma", Address: third.URL},
	)
	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()

	target, projectID, sandboxID, _, err := app.selectSandbox(cmd, sandboxB)
	if err != nil {
		t.Fatalf("selectSandbox(%s) error = %v", sandboxB, err)
	}
	if target.serverURL != other.URL || projectID != defaultProjectAlias || sandboxID != sandboxB {
		t.Fatalf("selectSandbox(%s) = %s %s %s, want beta's default project", sandboxB, target.serverURL, projectID, sandboxID)
	}

	target, projectID, _, _, err = app.selectSandbox(cmd, sandboxA)
	if err != nil || target != app || projectID != "project-1" {
		t.Fatalf("selectSandbox(%s) = %v, %s, %v, want the primary first", sandboxA, target, projectID, err)
	}

	if _, _, _, _, err := app.selectSandbox(cmd, "sbx_9qk5n25t2hh2rv0z"); err == nil || !strings.Contains(err.Error(), "any server") {
		t.Fatalf("selectSandbox(unknown) error = %v, want it to say no server has it", err)
	}
}

// A discobox's address names its server, and reaching the discobox there
// registers the server under the name it offers (ADR 0116 §6).
func TestSelectSandboxAddressRegistersItsServer(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	// discobox://<ip>:<port> is https.
	remote := httptest.NewTLSServer(fakeServerHandler("Gamma Box", sandboxC))
	t.Cleanup(remote.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = remote.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	server := "discobox://" + strings.TrimPrefix(remote.URL, "https://")

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, stderr := commandForTest()

	// A mistyped address leaves nothing behind.
	if _, _, _, _, err := app.selectSandbox(cmd, server+"/sbx_9qk5n25t2hh2rv0z"); err == nil {
		t.Fatal("selectSandbox() found a discobox the server does not have")
	}
	if reg, _ := loadServerRegistry(); len(reg.Servers) != 0 {
		t.Fatalf("registry = %+v after a miss, want nothing registered", reg.Servers)
	}

	target, _, sandboxID, _, err := app.selectSandbox(cmd, server+"/"+sandboxC)
	if err != nil {
		t.Fatalf("selectSandbox(address) error = %v", err)
	}
	if target.serverURL != server || sandboxID != sandboxC {
		t.Fatalf("selectSandbox(address) = %s %s", target.serverURL, sandboxID)
	}
	reg, err := loadServerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if want := []registeredServer{{Name: "gamma-box", Address: server}}; !reflect.DeepEqual(reg.Servers, want) {
		t.Fatalf("registry = %+v, want %+v", reg.Servers, want)
	}
	if !strings.Contains(stderr.String(), "Registered server gamma-box") {
		t.Fatalf("stderr = %q, want it to say the server was registered", stderr.String())
	}

	// Once is enough: the second time is an ordinary registered server.
	stderr.Reset()
	app = &App{serverURL: primary.URL, projectID: "project-1"}
	if _, _, _, _, err := app.selectSandbox(cmd, server+"/"+sandboxC); err != nil {
		t.Fatalf("selectSandbox(address) again error = %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q the second time, want nothing", stderr.String())
	}
}

func TestServersCommandRegistersRenamesAndRemoves(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	delta := fakeServer(t, "Delta Box")
	epsilon := fakeServer(t, "delta-box")

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := NewRootCommand()
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs(append([]string{"--server", primary.URL}, args...))
		err := cmd.Execute()
		return out.String(), err
	}

	if out, err := run("admin", "remote", "add", delta.URL); err != nil || out != "delta-box\n" {
		t.Fatalf("servers add = %q, %v, want the name it offers", out, err)
	}
	// Two servers offering one name are both registered, under two names.
	if out, err := run("admin", "remote", "add", epsilon.URL); err != nil || out != "delta-box-2\n" {
		t.Fatalf("servers add (same offered name) = %q, %v", out, err)
	}
	if _, err := run("admin", "remote", "add", delta.URL); err == nil {
		t.Fatal("servers add registered one server twice")
	}
	if _, err := run("admin", "remote", "add", deadServer(t)); err == nil {
		t.Fatal("servers add registered a server that does not answer")
	}
	if _, err := run("admin", "remote", "rename", "delta-box", "delta-box-2"); err == nil {
		t.Fatal("servers rename took a name already registered")
	}
	if _, err := run("admin", "remote", "rename", "delta-box", "lab"); err != nil {
		t.Fatalf("servers rename: %v", err)
	}
	if _, err := run("admin", "remote", "rm", "delta-box-2"); err != nil {
		t.Fatalf("servers rm: %v", err)
	}

	// Through the alias, which is the same command.
	out, err := run("admin", "remotes", "-o", "json")
	if err != nil {
		t.Fatalf("servers: %v", err)
	}
	var body struct {
		Servers []serverRow `json:"servers"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("servers -o json: %v\n%s", err, out)
	}
	want := []serverRow{
		{Name: "alpha", Address: primary.URL, Primary: true},
		{Name: "lab", Address: delta.URL, Registered: true},
	}
	if !reflect.DeepEqual(body.Servers, want) {
		t.Fatalf("servers = %+v, want %+v", body.Servers, want)
	}
}

// Making a registered server the primary is what every command without
// --server talks to from then on, and it registers nothing: the primary it
// replaces, here only ever named by --server, is left unregistered (ADR 0135).
func TestServersPrimaryIsWhatACommandWithoutServerTalksTo(t *testing.T) {
	useTempServersFile(t)
	t.Setenv(serverEnv, "")
	alpha := fakeServer(t, "alpha")
	lab := fakeServer(t, "lab")
	beta := fakeServer(t, "beta")
	registerForTest(t, registeredServer{Name: "alpha", Address: alpha.URL}, registeredServer{Name: "lab", Address: lab.URL})

	run := func(args ...string) (string, string, error) {
		t.Helper()
		cmd := NewRootCommand()
		var out, errOut strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), errOut.String(), err
	}
	listed := func(args ...string) []serverRow {
		t.Helper()
		out, _, err := run(append(args, "admin", "remote", "-o", "json")...)
		if err != nil {
			t.Fatalf("admin remote: %v", err)
		}
		var body struct {
			Servers []serverRow `json:"servers"`
		}
		if err := json.Unmarshal([]byte(out), &body); err != nil {
			t.Fatalf("admin remote -o json: %v\n%s", err, out)
		}
		return body.Servers
	}

	if _, _, err := run("--server", beta.URL, "admin", "remote", "primary", "nowhere"); err == nil {
		t.Fatal("admin remote primary took a server nobody registered")
	}
	_, errOut, err := run("--server", beta.URL, "admin", "remote", "primary", "lab")
	if err != nil {
		t.Fatalf("admin remote primary lab: %v", err)
	}
	if !strings.Contains(errOut, "here they still do") {
		t.Fatalf("admin remote primary said %q, want it to say --server still wins here", errOut)
	}

	want := []serverRow{
		{Name: "lab", Address: lab.URL, Primary: true, Registered: true},
		{Name: "alpha", Address: alpha.URL, Registered: true},
	}
	if got := listed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("servers with no --server = %+v, want %+v", got, want)
	}
	if out, _, err := run("admin", "remote", "primary"); err != nil || out != "lab\t"+lab.URL+"\n" {
		t.Fatalf("admin remote primary = %q, %v, want lab", out, err)
	}
	// --server still wins for the command it is given to, by name as well.
	if out, _, err := run("--server", "alpha", "admin", "remote", "primary"); err != nil || !strings.HasPrefix(out, "alpha\t") {
		t.Fatalf("--server alpha admin remote primary = %q, %v, want alpha", out, err)
	}
	t.Setenv(serverEnv, alpha.URL)
	if out, _, err := run("admin", "remote", "primary"); err != nil || !strings.HasPrefix(out, "alpha\t") {
		t.Fatalf("%s=alpha admin remote primary = %q, %v, want alpha", serverEnv, out, err)
	}
	t.Setenv(serverEnv, "")

	if _, _, err := run("admin", "remote", "rm", "lab"); err == nil || !strings.Contains(err.Error(), "primary") {
		t.Fatalf("admin remote rm of the primary error = %v, want a refusal", err)
	}
	// Renaming it keeps it the primary: what is recorded is its address.
	if _, _, err := run("admin", "remote", "rename", "lab", "workstation"); err != nil {
		t.Fatalf("admin remote rename: %v", err)
	}
	if out, _, err := run("admin", "remote", "primary"); err != nil || !strings.HasPrefix(out, "workstation\t") {
		t.Fatalf("admin remote primary after a rename = %q, %v", out, err)
	}

	if _, errOut, err := run("admin", "remote", "primary", "alpha"); err != nil || errOut != "" {
		t.Fatalf("admin remote primary alpha = %q, %v, want no notes", errOut, err)
	}
	want = []serverRow{
		{Name: "alpha", Address: alpha.URL, Primary: true, Registered: true},
		{Name: "workstation", Address: lab.URL, Registered: true},
	}
	if got := listed(); !reflect.DeepEqual(got, want) {
		t.Fatalf("servers after switching back = %+v, want %+v", got, want)
	}
	if _, _, err := run("admin", "remote", "rm", "workstation"); err != nil {
		t.Fatalf("admin remote rm of a server no longer the primary: %v", err)
	}
}

// --register-current registers the primary being replaced, here only ever
// named by --server, under the name it offers; one that does not answer fails
// the command with nothing written, as add does (ADR 0135).
func TestServersPrimaryRegisterCurrent(t *testing.T) {
	useTempServersFile(t)
	t.Setenv(serverEnv, "")
	lab := fakeServer(t, "lab")
	beta := fakeServer(t, "beta")
	down := deadServer(t)
	registerForTest(t, registeredServer{Name: "lab", Address: lab.URL})

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := NewRootCommand()
		var errOut strings.Builder
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(&errOut)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return errOut.String(), err
	}
	registry := func() serverRegistry {
		t.Helper()
		reg, err := loadServerRegistry()
		if err != nil {
			t.Fatal(err)
		}
		return reg
	}

	if _, err := run("--server", down, "--auto-start-server=false", "admin", "remote", "primary", "lab", "--register-current"); err == nil {
		t.Fatal("admin remote primary --register-current succeeded with a primary that does not answer")
	}
	// A name nobody registered is said to be one, not a primary that is down.
	if _, err := run("--server", down, "--auto-start-server=false", "admin", "remote", "primary", "nowhere", "--register-current"); err == nil || !strings.Contains(err.Error(), "no server named nowhere") {
		t.Fatalf("admin remote primary nowhere --register-current error = %v, want it to name the unregistered server", err)
	}
	unchanged := serverRegistry{Servers: []registeredServer{{Name: "lab", Address: lab.URL}}}
	if got := registry(); !reflect.DeepEqual(got, unchanged) {
		t.Fatalf("registry after a failed --register-current = %+v, want it unchanged", got)
	}
	if _, err := run("admin", "remote", "primary", "--register-current"); err == nil {
		t.Fatal("admin remote primary --register-current took no server to make the primary")
	}

	errOut, err := run("--server", beta.URL, "admin", "remote", "primary", "lab", "--register-current")
	if err != nil {
		t.Fatalf("admin remote primary lab --register-current: %v", err)
	}
	if !strings.Contains(errOut, "as beta") {
		t.Fatalf("admin remote primary --register-current said %q, want it to say beta was registered", errOut)
	}
	want := serverRegistry{Primary: lab.URL, Servers: []registeredServer{{Name: "lab", Address: lab.URL}, {Name: "beta", Address: beta.URL}}}
	if got := registry(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry = %+v, want %+v", got, want)
	}
	// Registered already, so nothing is registered twice.
	if _, err := run("--server", "beta", "admin", "remote", "primary", "lab", "--register-current"); err != nil {
		t.Fatalf("admin remote primary --register-current again: %v", err)
	}
	if got := registry(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry after a second --register-current = %+v, want %+v", got, want)
	}
}

// The launcher lists every server, names each row's, and sends what is done to
// a discobox to the server it was listed from.
func TestLauncherListsEveryServerAndRoutesToIt(t *testing.T) {
	useTempServersFile(t)
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	other := httptest.NewServer(fakeServerHandler("beta", sandboxB))
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	app := &App{serverURL: primary.URL, projectID: "project-1", source: "."}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := newAPIDataSource(context.Background(), app, client, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	session, err := ds.Session(context.Background())
	if err != nil {
		t.Fatalf("Session() error = %v", err)
	}
	if !reflect.DeepEqual(session.Servers, []string{"alpha", "beta"}) {
		t.Fatalf("Session().Servers = %v, want the primary then beta", session.Servers)
	}

	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	got := map[string]string{}
	for _, box := range listing.Sandboxes {
		got[box.ID] = box.Server
	}
	if !reflect.DeepEqual(got, map[string]string{sandboxA: "alpha", sandboxB: "beta"}) {
		t.Fatalf("List() = %v", got)
	}
	if ds.at(sandboxA) != ds {
		t.Fatal("the primary's discobox is not routed to the primary")
	}
	if at := ds.at(sandboxB); at.app.serverURL != other.URL || at.projectID != defaultProjectAlias {
		t.Fatalf("beta's discobox is routed to %s %s", at.app.serverURL, at.projectID)
	}

	// A server that stops answering is reported, not waited on.
	other.Close()
	listing, err = ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() with beta down error = %v", err)
	}
	if len(listing.Sandboxes) != 1 || !reflect.DeepEqual(listing.Unreachable, []string{"beta"}) {
		t.Fatalf("List() with beta down = %+v", listing)
	}
}

// The inbox is every server's (ADR 0131 §1): each request names the server
// it is waiting on, and a server-scoped call names its server to reach it.
func TestLauncherGathersEveryServersRequests(t *testing.T) {
	useTempServersFile(t)
	t.Setenv(hostid.EnvVar, "host_0123456789abcdef")
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	other := httptest.NewServer(fakeServerHandler("beta", sandboxB))
	t.Cleanup(other.Close)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	ds := launcherDataSource(t, primary.URL)
	requests, err := ds.CredentialRequests(context.Background())
	if err != nil {
		t.Fatalf("CredentialRequests() error = %v", err)
	}
	got := map[string]string{}
	for _, req := range requests {
		got[req.ID] = req.Server
	}
	want := map[string]string{"sreq_alpha_" + sandboxA: "alpha", "sreq_beta_" + sandboxB: "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CredentialRequests() = %v, want %v", got, want)
	}

	for _, name := range []string{"", "alpha"} {
		if on, err := ds.on(context.Background(), name); err != nil || on != ds {
			t.Fatalf("on(%q) = %v, %v, want the primary's own", name, on, err)
		}
	}
	on, err := ds.on(context.Background(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if on.app.serverURL != other.URL || on.projectID != defaultProjectAlias {
		t.Fatalf("on(beta) is aimed at %s %s", on.app.serverURL, on.projectID)
	}
	if _, err := ds.on(context.Background(), "gamma"); err == nil {
		t.Fatal("on() found a server the window does not list")
	}

	// A server that stops answering has nothing waiting as far as the window
	// can tell, and the rest are still the inbox.
	other.Close()
	requests, err = ds.CredentialRequests(context.Background())
	if err != nil {
		t.Fatalf("CredentialRequests() with beta down error = %v", err)
	}
	if len(requests) != 1 || requests[0].Server != "alpha" {
		t.Fatalf("CredentialRequests() with beta down = %+v, want alpha's alone", requests)
	}
}

// One server registered under two addresses lists its requests once, as it
// lists its discoboxes once: a request counted twice marks a row twice and sits
// in two servers' inboxes.
func TestLauncherListsARequestOnce(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	beta := fakeServer(t, "beta", sandboxB)
	again := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: beta.URL}, registeredServer{Name: "beta-2", Address: again.URL})

	ds := launcherDataSource(t, primary.URL)
	requests, err := ds.CredentialRequests(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var onB []string
	for _, req := range requests {
		if req.SandboxID == sandboxB {
			onB = append(onB, req.Server)
		}
	}
	if !reflect.DeepEqual(onB, []string{"beta"}) {
		t.Fatalf("beta's request is listed under %v, want once under beta", onB)
	}
}

// A primary that offers a registered server's name keeps its address in the
// window: names route configuration and approvals (on), and two servers under
// one would send one's approvals to the other.
func TestLauncherServerNamesAreUnique(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "beta", sandboxA)
	registered := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: registered.URL})

	ds := launcherDataSource(t, primary.URL)
	session, err := ds.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Servers) != 2 || session.Servers[0] == session.Servers[1] {
		t.Fatalf("Session().Servers = %v, want two different names", session.Servers)
	}
	on, err := ds.on(context.Background(), "beta")
	if err != nil {
		t.Fatal(err)
	}
	if on.app.serverURL != registered.URL {
		t.Fatalf("on(beta) is aimed at %s, want the registered beta", on.app.serverURL)
	}
}

// Answering a request takes it out of the inbox however slow its server is to
// list again: an answer that was in flight when the request was answered is
// discarded when it lands, rather than putting the request back.
func TestAnAnsweredRequestIsNotPutBackByAStaleRead(t *testing.T) {
	d := &apiDataSource{}
	s := &tuiServer{server: &server{name: "alpha", primary: true}, source: d}
	d.servers = []*tuiServer{s}
	req := tui.CredentialRequest{ID: "sreq_1"}
	poll := func(s *tuiServer) *serverPoll[tui.CredentialRequest] { return &s.requests }
	answer := func(context.Context, *apiDataSource) ([]tui.CredentialRequest, error) {
		return []tui.CredentialRequest{req}, nil
	}

	pollEveryServer(context.Background(), d, poll, answer)
	if len(s.requests.last) != 1 {
		t.Fatalf("first poll holds %v, want the request", s.requests.last)
	}

	// A read that is slower than the poll's patience, and so still out when
	// the request is answered.
	release, landed := make(chan struct{}), make(chan struct{})
	slow := func(context.Context, *apiDataSource) ([]tui.CredentialRequest, error) {
		defer close(landed)
		<-release
		return []tui.CredentialRequest{req}, nil
	}
	pollEveryServer(context.Background(), d, poll, slow)

	d.mu.Lock()
	stale(&s.requests, func(r tui.CredentialRequest) bool { return r.ID == req.ID })
	d.mu.Unlock()
	if len(s.requests.last) != 0 {
		t.Fatalf("after the answer the poll holds %v, want it gone", s.requests.last)
	}

	close(release)
	<-landed
	// The landing takes the lock just after the read returns.
	time.Sleep(100 * time.Millisecond)
	d.mu.Lock()
	got := append([]tui.CredentialRequest(nil), s.requests.last...)
	d.mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("the read taken before the answer put %v back", got)
	}
	none := func(context.Context, *apiDataSource) ([]tui.CredentialRequest, error) { return nil, nil }
	pollEveryServer(context.Background(), d, poll, none)
	if len(s.requests.last) != 0 {
		t.Fatalf("the next poll holds %v, want nothing waiting", s.requests.last)
	}
}

// hangingServer is a server that takes the connection and then thinks about
// it forever, counting what it was asked. Its requests are released before it
// is closed, so a test never waits on one it is done with.
func hangingServer(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	gone := make(chan struct{})
	var asked atomic.Int64
	// The port probe is not a client; see ignoringPortProbe.
	server := httptest.NewServer(ignoringPortProbe(func(_ http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		select {
		case <-gone:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(gone) })
	return server, func() int { return int(asked.Load()) }
}

// A server that hangs holds nothing up: the poll comes back with what the
// other servers said, and the next one does not stack a second request onto
// the server that has not finished the first.
func TestLauncherDoesNotWaitOnAServerThatHangs(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	hanging, asked := hangingServer(t)
	registerForTest(t, registeredServer{Name: "beta", Address: hanging.URL})

	ds := launcherDataSource(t, primary.URL)
	started := time.Now()
	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	// Its patience, and the slack a loaded test machine needs — nothing near
	// the leash the request itself is on (pollTimeout).
	if waited := time.Since(started); waited > listPatience+5*time.Second {
		t.Fatalf("List() waited %s on a server that hangs, want no longer than its patience", waited.Round(time.Millisecond))
	}
	if len(listing.Sandboxes) != 1 || listing.Sandboxes[0].ID != sandboxA {
		t.Fatalf("List() = %+v, want the server that answered", listing.Sandboxes)
	}
	// Not "not answering": it has not failed, it has not answered yet, and the
	// window says those two differently.
	if len(listing.Unreachable) != 0 {
		t.Fatalf("List() reported %v unreachable while it was still being asked", listing.Unreachable)
	}
	if !reflect.DeepEqual(listing.Waiting, []string{"beta"}) {
		t.Fatalf("List().Waiting = %v, want the server it is still asking", listing.Waiting)
	}

	for range 3 {
		listing, err := ds.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if !reflect.DeepEqual(listing.Waiting, []string{"beta"}) {
			t.Fatalf("List().Waiting = %v, want the request still in flight", listing.Waiting)
		}
	}
	if got := asked(); got != 1 {
		t.Fatalf("the hanging server was asked %d times, want the one request still in flight", got)
	}
}

// slowServer answers, eventually: a server that takes longer than a poll waits
// is not a server whose answer stops being wanted.
func slowServer(t *testing.T, name string, answerIn time.Duration, sandboxIDs ...string) *httptest.Server {
	t.Helper()
	answers := fakeServerHandler(name, sandboxIDs...)
	// The port probe is not a client; see ignoringPortProbe.
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(answerIn):
		case <-r.Context().Done():
			return
		}
		answers.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

// A server slower than a poll's patience is listed when it answers, not cut
// off at the patience and never listed at all: the poll gives up on it, the
// request does not, and the poll after it draws what came back.
func TestASlowServerIsListedWhenItAnswers(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	primary := fakeServer(t, "alpha", sandboxA)
	slow := slowServer(t, "beta", listPatience*2, sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: slow.URL})

	ds := launcherDataSource(t, primary.URL)
	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listing.Sandboxes) != 1 || !reflect.DeepEqual(listing.Waiting, []string{"beta"}) {
		t.Fatalf("List() = %+v, want the primary's row and beta still being asked", listing)
	}

	deadline := time.Now().Add(30 * time.Second)
	for !time.Now().After(deadline) {
		listing, err := ds.List(context.Background())
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(listing.Sandboxes) == 2 {
			return
		}
		if len(listing.Unreachable) > 0 {
			t.Fatalf("a server that was answering was reported as %v", listing.Unreachable)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("a server that answered late was never listed")
}

// The same for the only server there is, which the window does wait for:
// there is nothing to draw without it, so it is waited on however long it
// takes rather than cut off with an error it did not earn.
func TestTheOnlyServerIsWaitedForHoweverSlowItIs(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	slow := slowServer(t, "alpha", listPatience*2, sandboxA)

	ds := launcherDataSource(t, slow.URL)
	started := time.Now()
	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listing.Sandboxes) != 1 {
		t.Fatalf("List() = %+v, want the discobox the server answered with", listing)
	}
	if waited := time.Since(started); waited < listPatience {
		t.Fatalf("List() came back in %s without waiting for the answer", waited.Round(time.Millisecond))
	}
}

// The primary is a server like the others here: one that is down is reported
// as not answering, and the rest of the servers are the listing. A window that
// blanked every server because one of them went away would be a window that
// one bad server takes down.
func TestLauncherKeepsListingWhenThePrimaryIsDown(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	down := deadServer(t)
	other := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	ds := launcherDataSource(t, down)
	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() with the primary down error = %v, want the other servers listed", err)
	}
	if len(listing.Sandboxes) != 1 || listing.Sandboxes[0].Server != "beta" {
		t.Fatalf("List() = %+v, want beta's discobox", listing.Sandboxes)
	}
	if len(listing.Unreachable) != 1 || listing.Unreachable[0] == "beta" {
		t.Fatalf("List().Unreachable = %v, want the primary named", listing.Unreachable)
	}
}

// With one server there is no listing without it, so its failure is the
// listing's failure — what a window with one server has always reported.
func TestLauncherWithOneServerReportsItsFailure(t *testing.T) {
	useTempServersFile(t)
	t.Chdir(t.TempDir())
	ds := launcherDataSource(t, deadServer(t))
	if ds.servers != nil {
		t.Fatalf("one server was set up as %d", len(ds.servers))
	}
	if _, err := ds.List(context.Background()); err == nil {
		t.Fatal("List() with the only server down reported no error")
	}
}

// launcherDataSource is the window's data source aimed at a primary, the way
// runTUI builds one.
func launcherDataSource(t *testing.T, serverURL string) *apiDataSource {
	t.Helper()
	app := &App{serverURL: serverURL, projectID: "project-1", source: "."}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := newAPIDataSource(context.Background(), app, client, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

// A registration records the peer ID the server gives, which is what
// recognizes one server registered again under another address — its http
// address and its peer address, say.
func TestServersAddRecordsThePeerID(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha")
	peer := peerWithKeyByteForCLI(t, 0xaa).String()
	byHTTP := httptest.NewServer(fakePeerServerHandler("box", peer))
	t.Cleanup(byHTTP.Close)
	// A second address answering as the same peer.
	again := httptest.NewServer(fakePeerServerHandler("box", peer))
	t.Cleanup(again.Close)

	run := func(args ...string) (string, error) {
		t.Helper()
		cmd := NewRootCommand()
		var out, errOut strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs(append([]string{"--server", primary.URL}, args...))
		err := cmd.Execute()
		return out.String(), err
	}

	if _, err := run("admin", "remote", "add", byHTTP.URL); err != nil {
		t.Fatalf("servers add: %v", err)
	}
	reg, err := loadServerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if want := []registeredServer{{Name: "box", Address: byHTTP.URL, ID: peer}}; !reflect.DeepEqual(reg.Servers, want) {
		t.Fatalf("registry = %+v, want %+v", reg.Servers, want)
	}
	if _, err := run("admin", "remote", "add", again.URL); err == nil || !strings.Contains(err.Error(), "already registered as box") {
		t.Fatalf("servers add of the same peer at another address error = %v, want it refused", err)
	}

	out, err := run("admin", "remote", "-o", "json")
	if err != nil {
		t.Fatalf("servers: %v", err)
	}
	var body struct {
		Servers []serverRow `json:"servers"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("servers -o json: %v\n%s", err, out)
	}
	if len(body.Servers) != 2 || body.Servers[1].ID != peer || body.Servers[0].ID != "" {
		t.Fatalf("servers = %+v, want box with its peer ID and a primary with none", body.Servers)
	}
	table, err := run("admin", "remote")
	if err != nil {
		t.Fatalf("servers: %v", err)
	}
	if !strings.Contains(table, peer) || !strings.Contains(strings.SplitN(table, "\n", 2)[0], "ID") {
		t.Fatalf("servers =\n%s\nwant an ID column carrying %s", table, peer)
	}
}

func peerWithKeyByteForCLI(t *testing.T, b byte) endpoint.IrohID {
	t.Helper()
	var key [32]byte
	key[0] = b
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// status says who the server is as well as how it was reached, in the header
// above the layers.
func TestStatusPrintsTheServersPeerID(t *testing.T) {
	peer := peerWithKeyByteForCLI(t, 0xbb).String()
	var out strings.Builder
	printStatus(&out, statusReport{
		Endpoint:  endpoint.Diagnosis{Endpoint: "http://127.0.0.1:8081"},
		Peer:      &identityValue{PeerID: peer, Source: "the server"},
		Reachable: true,
	})
	if !strings.Contains(out.String(), "peer      "+peer+" (from the server)") {
		t.Fatalf("status =\n%s\nwant a peer row", out.String())
	}

	out.Reset()
	printStatus(&out, statusReport{
		Endpoint:  endpoint.Diagnosis{Endpoint: "unix:///tmp/discobox.sock"},
		Peer:      &identityValue{Source: "the server", Reason: "this server gives no peer ID, which only a server from before every server had one does"},
		Reachable: true,
	})
	if !strings.Contains(out.String(), "peer      none: this server gives no peer ID") {
		t.Fatalf("status =\n%s\nwant the peer row to say there is none", out.String())
	}
}

// useTempSSHMachine puts this machine's ssh files where the test can read them
// and nowhere near the developer's own.
func useTempSSHMachine(t *testing.T) string {
	t.Helper()
	setHome(t, t.TempDir())
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	return state
}

func managedSSHConfigs(t *testing.T, state string) map[string]string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(state, "discobox", "cli", "ssh", "*", "config"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(filepath.Dir(path))] = string(data)
	}
	return out
}

// `--write` covers every server, each into its own files, so one ssh reaches
// the discoboxes on all of them (ADR 0116 §4).
func TestSSHConfigWritesEveryServer(t *testing.T) {
	useTempServersFile(t)
	state := useTempSSHMachine(t)
	primary := fakeServer(t, "alpha", sandboxA)
	other := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", primary.URL, "--project", "project-1", "admin", "ssh-config", "--write"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("ssh-config --write: %v", err)
	}

	configs := managedSSHConfigs(t, state)
	if len(configs) != 2 {
		t.Fatalf("wrote %d configs, want one per server: %v", len(configs), configs)
	}
	if got := configs[fakeProjectID("alpha")]; !strings.Contains(got, sandboxA) || strings.Contains(got, sandboxB) {
		t.Fatalf("alpha's config carries the wrong discoboxes:\n%s", got)
	}
	if got := configs[fakeProjectID("beta")]; !strings.Contains(got, sandboxB) || strings.Contains(got, primary.URL) {
		t.Fatalf("beta's config carries the wrong discoboxes or the wrong server:\n%s", got)
	}
	// Each stanza reaches its own server, and ssh Includes both files.
	if got := configs[fakeProjectID("beta")]; !strings.Contains(got, other.URL) {
		t.Fatalf("beta's stanzas do not reach beta:\n%s", got)
	}
	userConfig, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".ssh", "config"))
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{fakeProjectID("alpha"), fakeProjectID("beta")} {
		if !strings.Contains(string(userConfig), project) {
			t.Fatalf("~/.ssh/config does not Include %s:\n%s", project, userConfig)
		}
	}
}

// Registering a server syncs its stanzas there and then: its discoboxes are
// listed from here now, so ssh reaches them from here too.
func TestServersAddSyncsItsSSHConfig(t *testing.T) {
	useTempServersFile(t)
	state := useTempSSHMachine(t)
	primary := fakeServer(t, "alpha")
	other := fakeServer(t, "beta", sandboxB)

	cmd := NewRootCommand()
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", primary.URL, "admin", "remote", "add", other.URL})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("servers add: %v", err)
	}

	configs := managedSSHConfigs(t, state)
	got, ok := configs[fakeProjectID("beta")]
	if !ok {
		t.Fatalf("registering beta wrote no config for it: %v", configs)
	}
	if !strings.Contains(got, sandboxB) || !strings.Contains(got, other.URL) {
		t.Fatalf("beta's config does not carry its discobox and its server:\n%s", got)
	}
}

// The lookup a name needs runs under the caller's context, so a resolver that
// never answers costs a listing the bound it set rather than one of its own,
// and an interrupt reaches it.
func TestResolvingANameObeysTheCallersContext(t *testing.T) {
	app := &App{serverURL: "discobox://nothing.invalid"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.resolveServerAddress(ctx); err == nil {
		t.Fatal("resolveServerAddress() ignored a canceled context")
	}
}

// One server is one server however it is reached: an address naming the peer a
// registration recorded is that registration, listed once (ADR 0116 §3).
func TestServersListsThePrimaryOnceByItsPeerID(t *testing.T) {
	useTempServersFile(t)
	peer := peerWithKeyByteForCLI(t, 0xcc).String()
	registerForTest(t, registeredServer{Name: "box", Address: "https://10.0.0.9:443", ID: peer})

	app := &App{serverURL: "discobox://" + peer}
	set, err := app.servers()
	if err != nil {
		t.Fatalf("servers() error = %v", err)
	}
	if len(set) != 1 {
		t.Fatalf("servers() = %d servers, want the primary alone", len(set))
	}
	if !set[0].primary || !set[0].registered || set[0].name != "box" || set[0].id != peer {
		t.Fatalf("primary = %+v, want it listed once as box", *set[0])
	}
}

// The primary's name is settled before the window opens, so the first listing
// stamps its rows with the name the session publishes them under.
func TestLauncherSettlesThePrimaryNameBeforeItListsAnything(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "Alpha", sandboxA)
	other := fakeServer(t, "beta", sandboxB)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})

	app := &App{serverURL: primary.URL, projectID: "project-1", source: "."}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	ds, err := newAPIDataSource(context.Background(), app, client, "project-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := ds.servers[0].name; got != "Alpha" {
		t.Fatalf("the primary is called %q before anything listed, want the name it offers", got)
	}
	listing, err := ds.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, box := range listing.Sandboxes {
		if box.ID == sandboxA && box.Server != "Alpha" {
			t.Fatalf("the primary's row says %q, want the name the session publishes", box.Server)
		}
	}
}

// shell takes a discobox's address like every other command that takes a
// discobox (ADR 0116 §6), rather than handing the address on as the command.
func TestShellTargetRoutesAnAddress(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	remote := fakeServer(t, "beta", sandboxB)
	address := "discobox+http://" + strings.TrimPrefix(remote.URL, "http://")

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()
	target, projectID, sandboxID, _, args, err := app.resolveShellTarget(cmd, []string{address + "/" + sandboxB, "ls", "-la"})
	if err != nil {
		t.Fatalf("resolveShellTarget(address) error = %v", err)
	}
	if target.serverURL != address || sandboxID != sandboxB || projectID != defaultProjectAlias {
		t.Fatalf("resolveShellTarget(address) = %s %s %s", target.serverURL, projectID, sandboxID)
	}
	if !reflect.DeepEqual(args, []string{"ls", "-la"}) {
		t.Fatalf("command = %v, want the address consumed as the discobox", args)
	}
}

// cp takes a discobox's address like every other command that takes a
// discobox (ADR 0116 §6): the operand splits at the colon that ends the
// discobox, not at the one in the scheme, and the copy runs against the server
// the address names — registering it on the way, as any other address does.
func TestCPRoutesAnAddress(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	remote := fakeServer(t, "beta", sandboxB)
	server := "discobox+http://" + strings.TrimPrefix(remote.URL, "http://")
	address := server + "/" + sandboxB

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()

	operands := parseCPOperands([]string{address + ":/tmp/x", "."})
	if !operands[0].remote || operands[0].reference != address || operands[0].path != "/tmp/x" {
		t.Fatalf("operand = %+v, want the address and the path after it", operands[0])
	}

	target, err := app.resolveCPTarget(cmd, operands)
	if err != nil {
		t.Fatalf("resolveCPTarget() error = %v", err)
	}
	if target.app.serverURL != server || target.projectID != defaultProjectAlias {
		t.Fatalf("the copy runs against %s in %s, want the server the address names", target.app.serverURL, target.projectID)
	}
	rewritten, err := target.app.resolveCPOperands(cmd, target.client, target.projectID, operands, target.resolved)
	if err != nil {
		t.Fatalf("resolveCPOperands() error = %v", err)
	}
	if !strings.HasPrefix(rewritten[0], sandboxB+"@") || !strings.HasSuffix(rewritten[0], ":/tmp/x") {
		t.Fatalf("operand = %q, want it rewritten for scp against that discobox", rewritten[0])
	}
	reg, err := loadServerRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Servers) != 1 || reg.Servers[0].Address != server {
		t.Fatalf("registry = %+v, want the address's server registered", reg.Servers)
	}
}

// One scp runs over one bridge, so a copy naming two servers is refused rather
// than half made.
func TestCPRefusesTwoServersInOneCopy(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	first := fakeServer(t, "beta", sandboxB)
	second := fakeServer(t, "gamma", sandboxC)

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()
	operands := parseCPOperands([]string{
		"discobox+http://" + strings.TrimPrefix(first.URL, "http://") + "/" + sandboxB + ":/tmp/x",
		"discobox+http://" + strings.TrimPrefix(second.URL, "http://") + "/" + sandboxC + ":/tmp/y",
	})
	if _, err := app.resolveCPTarget(cmd, operands); err == nil || !strings.Contains(err.Error(), "one copy reaches one server") {
		t.Fatalf("resolveCPTarget() error = %v, want two servers refused", err)
	}
	// Decided from the operands, so neither server was reached — and reaching
	// one through its address is what registers it (ADR 0116 §6).
	if reg, err := loadServerRegistry(); err != nil || len(reg.Servers) != 0 {
		t.Fatalf("registry = %+v (%v), want a refused copy to register nothing", reg.Servers, err)
	}
}

// A trailing colon is the discobox's home directory, exactly as `mybox:` is:
// the same copy to the same place must not depend on how the discobox was
// named.
func TestCPAddressTrailingColonIsTheHomeDirectory(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	remote := fakeServer(t, "beta", sandboxB)
	server := "discobox+http://" + strings.TrimPrefix(remote.URL, "http://")

	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()
	operands := parseCPOperands([]string{"x.txt", server + "/" + sandboxB + ":"})
	home := operands[1]
	if !home.remote || home.path != "" || home.addressWithoutPath {
		t.Fatalf("operand = %+v, want the home directory rather than a missing path", home)
	}
	target, err := app.resolveCPTarget(cmd, operands)
	if err != nil {
		t.Fatalf("resolveCPTarget() error = %v, want the home-directory form taken", err)
	}
	rewritten, err := target.app.resolveCPOperands(cmd, target.client, target.projectID, operands, target.resolved)
	if err != nil {
		t.Fatalf("resolveCPOperands() error = %v", err)
	}
	if want := sandboxB + "@" + sshBridgeHost + ":"; rewritten[1] != want {
		t.Fatalf("operand = %q, want %q", rewritten[1], want)
	}
}

// ?addr= carries host:port, so the colon after it is as likely the port's as
// the path's: the operand is refused by name rather than split somewhere
// plausible and wrong, and nothing is contacted to find that out.
func TestCPRefusesAnAddressCarryingAQuery(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", sandboxA)
	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()

	operands := parseCPOperands([]string{
		"discobox://" + peerWithKeyByteForCLI(t, 0xdd).String() + "/" + sandboxB + "?addr=10.0.0.5:4433:/tmp/x",
		".",
	})
	if !operands[0].addressWithQuery || operands[0].path != "" {
		t.Fatalf("operand = %+v, want a query the split does not guess past", operands[0])
	}
	_, err := app.resolveCPTarget(cmd, operands)
	if err == nil || !strings.Contains(err.Error(), "query") || !strings.Contains(err.Error(), "remote add") {
		t.Fatalf("resolveCPTarget() error = %v, want it refused with the way round it", err)
	}
	if reg, regErr := loadServerRegistry(); regErr != nil || len(reg.Servers) != 0 {
		t.Fatalf("registry = %+v (%v), want nothing registered", reg.Servers, regErr)
	}
}

// An address with nothing after it names a discobox but no file, and says so
// before anything is contacted.
func TestCPAddressNeedsAPath(t *testing.T) {
	useTempServersFile(t)
	app := &App{serverURL: "http://127.0.0.1:1", projectID: "project-1"}
	cmd, _ := commandForTest()
	operands := parseCPOperands([]string{"discobox://box.example.com/" + sandboxB, "."})
	if _, err := app.resolveCPTarget(cmd, operands); err == nil || !strings.Contains(err.Error(), "no file") {
		t.Fatalf("resolveCPTarget() error = %v, want a path demanded", err)
	}
}

// A lookup that failed is not what the App remembers. The context it failed
// under is the caller's — a listing's bound, an interrupt — while the App
// outlives that command: the launcher holds one per registered server for the
// life of the window, so a remembered failure would be a server that never
// came back.
func TestAFailedNameLookupIsNotRemembered(t *testing.T) {
	app := &App{serverURL: "discobox://nothing.invalid"}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := app.resolveServerAddress(canceled); err == nil {
		t.Fatal("resolveServerAddress() ignored a canceled context")
	}

	// Nothing was remembered, so the next caller looks the name up under its
	// own bound. Asserted on the App rather than by resolving again: a second
	// lookup would need a resolver to answer it, and a test that skips its own
	// point reads as coverage while proving nothing.
	if app.resolveDone || !reflect.DeepEqual(app.resolved, endpoint.Endpoint{}) {
		t.Fatalf("the App remembered a failed lookup: done=%v resolved=%#v", app.resolveDone, app.resolved)
	}
}

// A server that found more than one discobox answered, and which discoboxes it
// found is the answer the user needs. Filing that with the servers that could
// not be reached says the opposite happened and throws the matches away, so the
// same typo behaves differently depending on which server holds the boxes.
func TestAnAmbiguousShortIDOnAnotherServerSaysWhichDiscoboxes(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "alpha", "sbx_1t1t1t1t1t1t1t1t")
	other := fakeServer(t, "beta", sandboxB, sandboxC)
	registerForTest(t, registeredServer{Name: "beta", Address: other.URL})
	app := &App{serverURL: primary.URL, projectID: "project-1"}
	cmd, _ := commandForTest()

	_, _, _, _, err := app.selectSandbox(cmd, "sbx_9qk5n25t2hh2rv0")
	if err == nil {
		t.Fatal("an ambiguous short discobox ID resolved to one discobox")
	}
	if !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), sandboxB) || !strings.Contains(err.Error(), sandboxC) {
		t.Fatalf("error = %v, want it to name both discoboxes", err)
	}
	if strings.Contains(err.Error(), "did not") {
		t.Fatalf("error = %v, want beta reported as a server that answered", err)
	}
}

// A server older than tags ignores the tag parameter and lists everything. Its
// discoboxes carry no tags, so a tag filter lists none of them rather than all
// of them (ADR 0136).
func TestListEveryServerFiltersTagsAServerIgnored(t *testing.T) {
	useTempServersFile(t)
	primary := fakeServer(t, "Alpha", sandboxA, sandboxB)
	app := &App{serverURL: primary.URL, projectID: "project-1"}

	listed, _, err := app.listEveryServer(context.Background(), true, []string{"wip"})
	if err != nil {
		t.Fatalf("listEveryServer() error = %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("listed %d discoboxes, want none: none of them is tagged", len(listed))
	}
	if listed, _, _ := app.listEveryServer(context.Background(), true, nil); len(listed) != 2 {
		t.Fatalf("listed %d without a filter, want both", len(listed))
	}
}
