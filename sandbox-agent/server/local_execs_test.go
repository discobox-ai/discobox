package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/localexec"
	"github.com/discobox-ai/discobox/sandbox-agent/runuser"
)

func ptrInt64(v int64) *int64 { return &v }

// The local socket serves and creates only the sandbox's default user's
// execs: an exec running as anyone else is not there to list, attach to, type
// into or end (ADR 26-10-22-853 §2).
func TestTheLocalSocketServesOnlyTheDefaultUsersExecs(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	publicKey, _ := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	cfg.ExecDefaults.Username = "dev" // uid 1000 in the fixture
	built, err := newRouterAndManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	routes := built.localExecs.routes()
	ctx := context.Background()
	asRoot, err := built.execs.Create(ctx, execs.CreateRequest{Command: []string{"sh"}, User: &execs.User{Name: "root", UID: ptrInt64(0), GID: ptrInt64(0)}})
	if err != nil {
		t.Fatal(err)
	}
	rootByName, err := built.execs.Create(ctx, execs.CreateRequest{Command: []string{"sh"}, User: &execs.User{Name: "root"}})
	if err != nil {
		t.Fatal(err)
	}

	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		resp := httptest.NewRecorder()
		routes.ServeHTTP(resp, httptest.NewRequestWithContext(ctx, method, path, strings.NewReader(body)))
		return resp
	}
	resp := call(http.MethodPost, "/execs", `{"shell":true,"cols":80,"rows":24}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("create shell: %d %s", resp.Code, resp.Body)
	}
	var shell localexec.Exec
	if err := json.Unmarshal(resp.Body.Bytes(), &shell); err != nil {
		t.Fatal(err)
	}
	created, ok := built.execs.Get(shell.ID)
	if !ok {
		t.Fatalf("no exec %s", shell.ID)
	}
	if created.User != nil && (created.User.UID == nil || *created.User.UID != 1000) {
		t.Fatalf("the shell runs as %+v, not the default user", created.User)
	}

	resp = call(http.MethodGet, "/execs", "")
	var listed []localexec.Exec
	if err := json.Unmarshal(resp.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, exec := range listed {
		ids[exec.ID] = true
	}
	if !ids[shell.ID] || ids[asRoot.ID] || ids[rootByName.ID] {
		t.Fatalf("listed %v; want the shell and neither root exec", ids)
	}
	for _, id := range []string{asRoot.ID, rootByName.ID} {
		for _, req := range [][2]string{{http.MethodPost, "/execs/" + id + "/start"}, {http.MethodPost, "/execs/" + id + "/input"}, {http.MethodDelete, "/execs/" + id}, {http.MethodGet, "/execs/" + id + "/attach"}} {
			if resp := call(req[0], req[1], `{"text":"id"}`); resp.Code != http.StatusNotFound {
				t.Fatalf("%s %s on a root exec: %d, want 404", req[0], req[1], resp.Code)
			}
		}
	}
}

// Every process in the sandbox can connect, so a connection is served only
// from root or the sandbox's default user, by the uid the kernel recorded.
func TestTheLocalSocketAdmitsOnlyRootAndTheDefaultUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	publicKey, _ := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	cfg.ExecDefaults.Username = "dev" // uid 1000 in the fixture
	built, err := newRouterAndManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admit := built.localExecs.admit(built.localExecs.routes())
	for _, tc := range []struct {
		name string
		uid  *uint32
		want int
	}{
		{"root", ptrUint32(0), http.StatusOK},
		{"the default user", ptrUint32(1000), http.StatusOK},
		{"another account", ptrUint32(999), http.StatusForbidden},
		{"a peer that could not be read", nil, http.StatusForbidden},
	} {
		ctx := context.Background()
		if tc.uid != nil {
			ctx = context.WithValue(ctx, peerUIDKey{}, *tc.uid)
		}
		resp := httptest.NewRecorder()
		admit.ServeHTTP(resp, httptest.NewRequestWithContext(ctx, http.MethodGet, "/execs", nil))
		if resp.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, resp.Code, tc.want)
		}
	}
}

func ptrUint32(v uint32) *uint32 { return &v }

// A manifest that names the sandbox user without a uid still lets that user
// in: the socket compares against the resolved user, not the declared one.
func TestTheLocalSocketAdmitsAUserNamedWithoutAUID(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	publicKey, _ := sandboxAgentTestSigner(t)
	cfg := testConfigWithRunner(publicKey, &sandboxAgentFakeRunner{})
	cfg.ExecDefaults.Username = "dev" // uid 1000 in the fixture
	built, err := newRouterAndManager(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !built.localExecs.admitted(1000) {
		t.Fatal("dev, named without a uid, was refused")
	}
	if built.localExecs.admitted(1001) {
		t.Fatal("another uid was admitted")
	}
}
