package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

const bindTestSecretJSON = `{"id":"sec_1github","projectId":"project-1","name":"github","type":"token","maxBindingTTLSeconds":0,` +
	`"routes":[{"host":"api.github.com"}],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`

const bindTestUseBinding = `{"id":"sbd_1","projectId":"project-1","secretId":"sec_1github","secretName":"github",` +
	`"targetKind":"project","targetId":"project-1","use":true,"delegate":false,"envVar":"GH_TOKEN","routes":[],` +
	`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`

// bindTestListing is what the binding listing answers; tests replace it.
type bindTestListing struct {
	bindings string
	patched  []map[string]any
}

// bindTestServer answers what `secret bind`, `unbind` and `bound` ask: the
// binding a create sends back, the secret it names, a listing and a delete.
func bindTestServer(t *testing.T, posted *map[string]any, deleted *[]string) *httptest.Server {
	t.Helper()
	return bindTestServerListing(t, posted, deleted, &bindTestListing{bindings: bindTestUseBinding})
}

func bindTestServerListing(t *testing.T, posted *map[string]any, deleted *[]string, listing *bindTestListing) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secret-bindings":
			if err := json.NewDecoder(r.Body).Decode(posted); err != nil {
				t.Fatalf("decode binding body: %v", err)
			}
			envVar, _ := (*posted)["envVar"].(string)
			use := envVar != ""
			_, _ = w.Write([]byte(`{"id":"sbd_1","projectId":"project-1","secretId":"sec_1github","secretName":"github",` +
				`"targetKind":"project","targetId":"project-1","use":` + map[bool]string{true: "true", false: "false"}[use] +
				`,"delegate":false,"envVar":"` + envVar + `","routes":[{"host":"api.github.com","path":"/repos/acme"}],` +
				`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets/sec_1github":
			_, _ = w.Write([]byte(bindTestSecretJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-bindings":
			if r.URL.Query().Get("secret") != "github" || r.URL.Query().Get("targetKind") != "project" {
				t.Fatalf("listing query = %s, want the secret on the project", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"secretBindings":[` + listing.bindings + `]}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/projects/project-1/secret-bindings/sbd_1":
			var patch map[string]any
			if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
				t.Fatalf("decode patch: %v", err)
			}
			listing.patched = append(listing.patched, patch)
			_, _ = w.Write([]byte(bindTestUseBinding))
		case r.Method == http.MethodDelete && r.URL.Path == "/projects/project-1/secret-bindings/sbd_1":
			*deleted = append(*deleted, "sbd_1")
			_, _ = w.Write([]byte(`{"deleted":["sbd_1","sbd_2","sbd_3"]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/bound-secrets":
			if r.URL.Query().Get("targetKind") != "project" {
				t.Fatalf("bound query = %s, want the project", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"boundSecrets":[{"secretId":"sec_1github","secretName":"github","bindingId":"sbd_1","from":"project",` +
				`"fromId":"project-1","use":true,"delegate":false,"envVar":"GH_TOKEN","routes":[[{"host":"api.github.com"}],[{"host":"api.github.com","path":"/repos/acme"}]]}]}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runBindCommand(t *testing.T, server *httptest.Server, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs(append([]string{"--server", server.URL, "--project", "project-1", "secret"}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// A bind says what it did in a sentence: what the target now holds, where it
// may be sent, and for how long — not a record of IDs.
func TestSecretBindSaysWhatItDid(t *testing.T) {
	var posted map[string]any
	server := bindTestServer(t, &posted, new([]string))

	out, err := runBindCommand(t, server, "bind", "github", "--project-wide", "--as", "GH_TOKEN", "--route", "api.github.com/repos/acme", "--for", "forever")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	want := map[string]any{
		"secret": "github", "targetKind": "project", "use": true, "envVar": "GH_TOKEN",
		"routes": []any{map[string]any{"host": "api.github.com", "path": "/repos/acme"}}, "ttlSeconds": float64(0),
	}
	if !reflect.DeepEqual(posted, want) {
		t.Fatalf("posted = %#v, want %#v", posted, want)
	}
	if !strings.Contains(out, "github is now GH_TOKEN in every discobox of the project, sent to api.github.com/repos/acme (within the secret's api.github.com); it never lapses.") {
		t.Fatalf("output = %q, want the sentence", out)
	}
	if strings.Contains(out, "sbd_1") || strings.Contains(out, "discobox-access env") {
		t.Fatalf("output = %q, want no IDs, and the shell hint only for a discobox", out)
	}
}

// Left out, the variable is the secret's name upper-snaked, as for `run --bind`.
func TestSecretBindNamesTheVariableAfterTheSecret(t *testing.T) {
	var posted map[string]any
	server := bindTestServer(t, &posted, new([]string))

	if _, err := runBindCommand(t, server, "bind", "github", "--project-wide"); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if posted["envVar"] != "GITHUB" || posted["use"] != true {
		t.Fatalf("posted = %#v, want GITHUB", posted)
	}
	if _, err := runBindCommand(t, server, "bind", "github", "--project-wide", "--harness", "claude-code"); err == nil {
		t.Fatal("two targets were accepted")
	}
}

// Unbinding names what went with it: the bindings a discobox handed on under
// the one taken back.
func TestSecretUnbindSaysWhatElseWent(t *testing.T) {
	var deleted []string
	server := bindTestServer(t, new(map[string]any), &deleted)

	out, err := runBindCommand(t, server, "unbind", "github", "--project-wide")
	if err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if len(deleted) != 1 {
		t.Fatalf("deleted = %v, want the one binding", deleted)
	}
	if !strings.Contains(out, "github (GH_TOKEN) taken back from every discobox of the project, and from the 2 discoboxes it was handed on to.") {
		t.Fatalf("output = %q", out)
	}
	if _, err := runBindCommand(t, server, "unbind", "github", "--project-wide", "--as", "OTHER"); err == nil || !strings.Contains(err.Error(), "not bound") {
		t.Fatalf("unbind of a variable nothing holds = %v, want it refused", err)
	}
}

// `bound` is the resolved view: each variable, its secret, where it comes
// from, and every route list a request carrying it is held to.
func TestSecretBoundShowsTheResolvedView(t *testing.T) {
	server := bindTestServer(t, new(map[string]any), new([]string))

	out, err := runBindCommand(t, server, "bound")
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	for _, want := range []string{"VARIABLE", "GH_TOKEN", "github", "project", "api.github.com/repos/acme within api.github.com", "forever"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want %q", out, want)
		}
	}
}

// A refused approval exits non-zero with the server's reason, so a script
// answering requests knows it did not take.
func TestSecretRequestApproveFailsWhenRefused(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"secretRequests":[{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"pending",` +
				`"routes":[{"host":"api.github.com"}],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}]}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/secret-requests/sreq_1/approve" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"status":422,"title":"Unprocessable Entity","detail":"github allows at most 1h"}`))
	}))
	t.Cleanup(server.Close)

	_, err := runBindCommand(t, server, "request", "approve", "sreq_1", "--for", "1w")
	if err == nil || !strings.Contains(err.Error(), "at most 1h") {
		t.Fatalf("err = %v, want the refusal", err)
	}
}

// A binding that both delivers a variable and may be given on loses only the
// half asked for; a delegation alone is not taken back by a plain unbind.
func TestSecretUnbindTakesBackOneHalf(t *testing.T) {
	both := strings.Replace(bindTestUseBinding, `"delegate":false`, `"delegate":true`, 1)
	var deleted []string
	listing := &bindTestListing{bindings: both}
	server := bindTestServerListing(t, new(map[string]any), &deleted, listing)

	out, err := runBindCommand(t, server, "unbind", "github", "--project-wide", "--delegate")
	if err != nil {
		t.Fatalf("unbind --delegate: %v", err)
	}
	if len(deleted) != 0 || len(listing.patched) != 1 || !reflect.DeepEqual(listing.patched[0], map[string]any{"delegate": false}) {
		t.Fatalf("deleted = %v, patched = %v; want only the delegation turned off", deleted, listing.patched)
	}
	if !strings.Contains(out, "may no longer be given on") || !strings.Contains(out, "stays in GH_TOKEN") {
		t.Fatalf("output = %q", out)
	}

	if _, err := runBindCommand(t, server, "unbind", "github", "--project-wide"); err != nil {
		t.Fatalf("unbind: %v", err)
	}
	if len(deleted) != 0 || len(listing.patched) != 2 || !reflect.DeepEqual(listing.patched[1], map[string]any{"use": false}) {
		t.Fatalf("deleted = %v, patched = %v; want only the variable turned off", deleted, listing.patched)
	}

	listing.bindings = strings.Replace(strings.Replace(bindTestUseBinding, `"use":true`, `"use":false`, 1), `"delegate":false`, `"delegate":true`, 1)
	_, err = runBindCommand(t, server, "unbind", "github", "--project-wide")
	if err == nil || !strings.Contains(err.Error(), "only given on") || !strings.Contains(err.Error(), "--delegate") {
		t.Fatalf("unbind of a delegation alone = %v, want it to say so", err)
	}
}

// `bound` names where a binding comes from as a person does — the discobox's
// name — and escapes what a discobox wrote: a child's variable and routes.
func TestSecretBoundNamesAndEscapes(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/bound-secrets":
			_, _ = w.Write([]byte(`{"boundSecrets":[{"secretId":"sec_1github","secretName":"github","bindingId":"sbd_1","from":"sandbox",` +
				`"fromId":"sbx_nfkv7n8v46gvn7t2","use":true,"delegate":false,"envVar":"EVIL\u001b[2J","routes":[[{"host":"x\u001b]0;pwned\u0007.example"}]]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes/sbx_nfkv7n8v46gvn7t2":
			_, _ = w.Write([]byte(`{"id":"sbx_nfkv7n8v46gvn7t2","projectId":"project-1","createdByUserId":"user-1","displayName":"childbox",` +
				`"config":{"name":"childbox","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},` +
				`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	out, err := runBindCommand(t, server, "bound", "--project-wide")
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if !strings.Contains(out, "discobox childbox") || strings.Contains(out, "sbx_nfkv7n8v46gvn7t2") {
		t.Fatalf("output = %q, want the discobox by name", out)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) {
		t.Fatalf("output = %q, want what the discobox wrote escaped", out)
	}
}

// --route is one route per flag everywhere, read as `secret create --route`
// reads one; a comma-joined list is refused rather than read as a host.
func TestApproveTakesOneRoutePerFlag(t *testing.T) {
	var approved map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests":
			_, _ = w.Write([]byte(`{"secretRequests":[{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"pending",` +
				`"routes":[],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secret-requests/sreq_1/approve":
			approved = nil
			if err := json.NewDecoder(r.Body).Decode(&approved); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"approved",` +
				`"routes":[],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	if _, err := runBindCommand(t, server, "request", "approve", "sreq_1", "--route", "api.github.com", "--route", "github.com/repos=blocked"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	want := []any{map[string]any{"host": "api.github.com"}, map[string]any{"host": "github.com", "path": "/repos", "blocked": true}}
	if !reflect.DeepEqual(approved["routes"], want) {
		t.Fatalf("routes = %#v, want %#v", approved["routes"], want)
	}
	if _, err := runBindCommand(t, server, "request", "approve", "sreq_1", "--route", "a.example,b.example"); err == nil {
		t.Fatal("a comma-joined --route was accepted")
	}
}

// `admin binding update` turns either half of a binding off with --use=false
// or --delegate=false, and spells the variable and lifetime as the person's
// verbs do: --as and --for.
func TestAdminBindingUpdateSpellsThePersonsWords(t *testing.T) {
	var patched map[string]any
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/projects/project-1/secret-bindings/sbd_1" {
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(bindTestUseBinding))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "binding", "update", "sbd_1",
		"--as", "GH_TOKEN", "--for", "1h", "--delegate=false"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("update: %v", err)
	}
	want := map[string]any{"envVar": "GH_TOKEN", "ttlSeconds": float64(3600), "delegate": false}
	if !reflect.DeepEqual(patched, want) {
		t.Fatalf("patched = %#v, want %#v", patched, want)
	}
}

// Approving says what was granted — the binding's routes and lifetime, which
// may be narrower than what the agent asked for — not the ask.
func TestApproveSaysWhatWasGranted(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-requests":
			_, _ = w.Write([]byte(`{"secretRequests":[{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"pending",` +
				`"routes":[{"host":"172.17.0.1"}],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secret-requests/sreq_1/approve":
			_, _ = w.Write([]byte(`{"id":"sreq_1","projectId":"project-1","requestedBy":"agent","status":"approved","sandboxId":"sbx_nfkv7n8v46gvn7t2",` +
				`"bindingId":"sbd_1","routes":[{"host":"172.17.0.1"}],"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secret-bindings/sbd_1":
			_, _ = w.Write([]byte(`{"id":"sbd_1","projectId":"project-1","secretId":"sec_1github","secretName":"github",` +
				`"targetKind":"sandbox","targetId":"sbx_nfkv7n8v46gvn7t2","use":true,"delegate":false,"envVar":"GH_TOKEN",` +
				`"routes":[{"host":"172.17.0.1","path":"/ok"}],"expiresAt":"2999-01-01T00:00:00Z",` +
				`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:00Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets/sec_1github":
			_, _ = w.Write([]byte(bindTestSecretJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/sandboxes/sbx_nfkv7n8v46gvn7t2":
			_, _ = w.Write([]byte(`{"id":"sbx_nfkv7n8v46gvn7t2","projectId":"project-1","createdByUserId":"user-1","displayName":"childbox",` +
				`"config":{"name":"childbox","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},` +
				`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	out, err := runBindCommand(t, server, "request", "approve", "sreq_1", "--route", "172.17.0.1/ok", "--for", "1h")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.Contains(out, "github is now GH_TOKEN in childbox, sent to 172.17.0.1/ok") || !strings.Contains(out, "it lasts") {
		t.Fatalf("output = %q, want what was granted", out)
	}
}

// `admin box create -s` says where each inline secret is sent once the create
// has succeeded, and nothing for one that failed.
func TestAdminBoxCreateReportsInlineSecretsOnlyOnceCreated(t *testing.T) {
	for _, fail := range []bool{true, false} {
		server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/skills-sources" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"skillsSources":[]}`))
				return
			}
			if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/sandboxes" {
				t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			if fail {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":400,"title":"Bad Request","detail":"no"}`))
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"sbx_nfkv7n8v46gvn7t2","projectId":"project-1","createdByUserId":"user-1","displayName":"box",` +
				`"config":{"name":"box","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},` +
				`"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		}))
		cmd := NewRootCommand()
		var errOut bytes.Buffer
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "box", "create", "--name", "box", "-s", "DB_PASS=hunter2@corp.example"})
		err := cmd.Execute()
		server.Close()
		said := strings.Contains(errOut.String(), "DB_PASS: stored, sent to corp.example")
		if fail && (err == nil || said) {
			t.Fatalf("failed create: err = %v, stderr = %q; want the failure and nothing said stored", err, errOut.String())
		}
		if !fail && (err != nil || !said) {
			t.Fatalf("create: err = %v, stderr = %q; want where DB_PASS is sent", err, errOut.String())
		}
	}
}

// A discobox names itself, so the name an approval prints is escaped in
// every sentence that carries it (cli/REVIEW.md).
func TestWriteBoundEscapesTheDiscoboxName(t *testing.T) {
	binding := &apimodel.SecretBinding{SecretName: apiclientgen.NewOptString("github"), Use: true, Delegate: true, EnvVar: apiclientgen.NewOptString("GH_TOKEN")}
	var out bytes.Buffer
	if err := writeBound(&out, binding, &apimodel.Secret{Name: "github"}, bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindSandbox, said: "box\x1b[2J"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(out.String(), 0x1b) || strings.Count(out.String(), `box\x1b[2J`) != 3 {
		t.Fatalf("output = %q, want the name escaped in all three sentences", out.String())
	}
}
