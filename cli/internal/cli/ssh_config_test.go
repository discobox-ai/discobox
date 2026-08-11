package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// sshConfigFakeServer serves the routes ssh-config drives: the SSH ingress
// discovery document, the project's enrolled keys, and its sandboxes.
type sshConfigFakeServer struct {
	ingress   string
	sandboxes []sshConfigFakeSandbox

	mu       sync.Mutex
	keys     []map[string]any
	enrolled []string // public key lines POSTed during the test
}

type sshConfigFakeSandbox struct {
	id   string
	name string
}

func (f *sshConfigFakeServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/ssh":
			_, _ = w.Write([]byte(f.ingress))
		case r.URL.Path == "/projects/project-1/ssh-keys" && r.Method == http.MethodGet:
			keys := f.keys
			if keys == nil {
				keys = []map[string]any{} // a nil slice marshals to null, which the schema rejects
			}
			body, _ := json.Marshal(map[string]any{"sshKeys": keys})
			_, _ = w.Write(body)
		case r.URL.Path == "/projects/project-1/ssh-keys" && r.Method == http.MethodPost:
			var payload struct {
				PublicKey string `json:"publicKey"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.enrolled = append(f.enrolled, payload.PublicKey)
			_, _ = w.Write([]byte(`{"id":"sshkey_1","projectId":"project-1",
				"publicKey":"` + payload.PublicKey + `","fingerprint":"SHA256:whatever",
				"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		case r.URL.Path == "/projects/project-1/sandboxes":
			_, _ = w.Write([]byte(f.sandboxesJSON()))
		default:
			t.Errorf("unexpected path %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (f *sshConfigFakeServer) sandboxesJSON() string {
	entries := make([]string, 0, len(f.sandboxes))
	for _, sandbox := range f.sandboxes {
		entries = append(entries, fmt.Sprintf(`{"id":%q,"projectId":"project-1","createdByUserId":"user-1",
			"config":{"name":%q,"image":"discobox-sandbox-agent:local","cpuVcpus":1,"memoryBytes":1,"storageBytes":1},
			"runtime":{"desiredState":"present","state":"running","generation":1,"observedGeneration":1},
			"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`, sandbox.id, sandbox.name))
	}
	return `{"sandboxes":[` + strings.Join(entries, ",") + `]}`
}

const sshConfigEnabledIngress = `{"enabled":true,"address":"ssh.example.com:3222","hostKey":"ssh-ed25519 AAAAfakehostkey=="}`

// runSSHConfig executes ssh-config against fake, always with an identity file
// inside the test's temp dir so no test can read or write the real one.
func runSSHConfig(t *testing.T, fake *sshConfigFakeServer, extraArgs ...string) (stdout, stderr string, err error) {
	t.Helper()
	server := fake.start(t)
	identity := filepath.Join(t.TempDir(), "id_ed25519")
	// Belt and braces alongside --identity-file: cliStateDir honours
	// XDG_STATE_HOME, so even a future test that forgets the flag cannot
	// generate or enrol a key in the developer's real state directory.
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(append([]string{
		"--server", server.URL, "--project", "project-1", "box", "ssh-config",
		"--identity-file", identity,
	}, extraArgs...))
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestSSHConfigEmitsAFriendlyNameAndTheID covers the two things a Host pattern
// has to be: friendly to type, and unambiguous. The ID stays as a second
// pattern so a renamed or duplicated sandbox is still reachable.
func TestSSHConfigEmitsAFriendlyNameAndTheID(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "cheerful_poincare"}},
	}
	out, _, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	for _, want := range []string{
		// Bare first: it is what anyone actually types. The qualified alias
		// stays as the unambiguous spelling.
		"Host cheerful_poincare cheerful_poincare.discobox.internal sbx_devbox00000001 sbx_devbox00000001.discobox.internal\n",
		"    HostName ssh.example.com\n",
		"    Port 3222\n",
		// The name is cosmetic; the ID is what routes.
		"    User sbx_devbox00000001\n",
		"    IdentitiesOnly yes\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q; got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "    IdentityFile ") {
		t.Fatalf("output has no IdentityFile:\n%s", out)
	}
}

// TestSSHConfigDropsAmbiguousAndUnsafeNames: ssh applies the first matching
// Host block, so a duplicated name would silently route to one sandbox for
// both, and a name with a glob or a space would match hosts it should not.
func TestSSHConfigDropsAmbiguousAndUnsafeNames(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress: sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{
			{id: "sbx_dup0000000001", name: "twin"},
			{id: "sbx_dup0000000002", name: "twin"},
			{id: "sbx_glob000000001", name: "*"},
			{id: "sbx_space00000001", name: "two words"},
			{id: "sbx_fine000000001", name: "unique_name"},
		},
	}
	out, _, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	if strings.Contains(out, "twin") {
		t.Fatalf("a duplicated name must not become a Host pattern:\n%s", out)
	}
	if strings.Contains(out, "*") {
		t.Fatalf("a glob name must never become a Host pattern:\n%s", out)
	}
	if strings.Contains(out, "two words") || strings.Contains(out, "Host words") {
		t.Fatalf("a name with whitespace must not become a Host pattern:\n%s", out)
	}
	if !strings.Contains(out, "Host unique_name unique_name.discobox.internal sbx_fine000000001 sbx_fine000000001.discobox.internal\n") {
		t.Fatalf("the unique, safe name should still be an alias:\n%s", out)
	}
	for _, id := range []string{"sbx_dup0000000001", "sbx_dup0000000002", "sbx_glob000000001", "sbx_space00000001"} {
		if !strings.Contains(out, "Host "+id+" "+id+".discobox.internal\n") {
			t.Fatalf("%s lost its ID patterns and is now unreachable:\n%s", id, out)
		}
	}
}

// TestSSHConfigGeneratesAndEnrollsAKey is the point of the automation: one
// command turns a fresh machine into one that can ssh.
func TestSSHConfigGeneratesAndEnrollsAKey(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	_, stderr, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	if len(fake.enrolled) != 1 {
		t.Fatalf("enrolled %d keys, want 1", len(fake.enrolled))
	}
	if !strings.HasPrefix(fake.enrolled[0], "ssh-ed25519 ") {
		t.Fatalf("enrolled key is not an ed25519 public key line: %q", fake.enrolled[0])
	}
	// Creating a credential is not something to do silently.
	if !strings.Contains(stderr, "generated a new SSH key") {
		t.Fatalf("key generation was not reported, stderr: %q", stderr)
	}
}

// TestSSHConfigDoesNotReEnrollAKnownKey keys enrollment on the fingerprint, so
// repeated runs do not pile up duplicate keys on the project.
func TestSSHConfigDoesNotReEnrollAKnownKey(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	server := fake.start(t)
	identity := filepath.Join(t.TempDir(), "id_ed25519")
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	run := func() {
		cmd := NewRootCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config",
			"--identity-file", identity})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute ssh-config: %v", err)
		}
	}

	run()
	if len(fake.enrolled) != 1 {
		t.Fatalf("first run enrolled %d keys, want 1", len(fake.enrolled))
	}
	// Reflect the enrollment back the way the server would on the next call.
	line, _, err := loadOrCreateSSHIdentity(identity)
	if err != nil {
		t.Fatalf("read identity: %v", err)
	}
	parsed := mustFingerprint(t, line)
	fake.mu.Lock()
	fake.keys = []map[string]any{{
		"id": "sshkey_1", "projectId": "project-1", "publicKey": line, "fingerprint": parsed,
		"createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
	}}
	fake.mu.Unlock()

	run()
	if len(fake.enrolled) != 1 {
		t.Fatalf("second run enrolled the key again: %d enrollments", len(fake.enrolled))
	}
}

func TestSSHConfigFlagsOverrideTheAdvertisedAddress(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	out, _, err := runSSHConfig(t, fake, "--host", "127.0.0.1", "--port", "22022")
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	for _, want := range []string{
		"    HostName 127.0.0.1\n",
		"    Port 22022\n",
		"[127.0.0.1]:22022 ssh-ed25519 AAAAfakehostkey==",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ssh.example.com") {
		t.Fatalf("advertised address leaked past the overrides:\n%s", out)
	}
}

// TestSSHConfigReportsDisabledIngress: SSH is opt-in, so "not enabled" is an
// ordinary answer the document carries, and the client should say so rather
// than emit a config pointing nowhere.
func TestSSHConfigReportsDisabledIngress(t *testing.T) {
	fake := &sshConfigFakeServer{ingress: `{"enabled":false}`}
	_, _, err := runSSHConfig(t, fake)
	if err == nil {
		t.Fatal("expected ssh-config to fail when the server has no SSH ingress")
	}
	if !strings.Contains(err.Error(), "DISCOBOX_SSH_LISTEN") {
		t.Fatalf("error should name the setting that enables SSH, got: %v", err)
	}
}

func TestKnownHostsHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		port int
		want string
	}{
		{host: "ssh.example.com", port: 3222, want: "[ssh.example.com]:3222"},
		{host: "::1", port: 3222, want: "[::1]:3222"},
		// ssh looks a port-22 host up under its bare name, so bracketing it
		// would write an entry that never matches.
		{host: "ssh.example.com", port: 22, want: "ssh.example.com"},
	} {
		if got := knownHostsHost(tc.host, tc.port); got != tc.want {
			t.Errorf("knownHostsHost(%q, %d) = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

// TestSSHConfigBareNameIsUsable is the shape a user actually types: `ssh
// <name>`, with no domain. Dropping the suffix costs the guarantee that a
// generated pattern cannot collide with a real host, which is why the
// qualified alias is still emitted next to it.
func TestSSHConfigBareNameIsUsable(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress:   sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{{id: "sbx_devbox00000001", name: "devbox"}},
	}
	out, _, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	patterns := strings.Fields(strings.SplitN(strings.TrimPrefix(out, "Host "), "\n", 2)[0])
	want := []string{"devbox", "devbox.discobox.internal", "sbx_devbox00000001", "sbx_devbox00000001.discobox.internal"}
	if len(patterns) != len(want) {
		t.Fatalf("Host patterns = %v, want %v", patterns, want)
	}
	for i, pattern := range want {
		if patterns[i] != pattern {
			t.Fatalf("Host patterns = %v, want %v", patterns, want)
		}
	}
}

// TestSSHConfigDropsANameThatSpellsAnotherSandboxesPattern is the collision the
// bare form newly allows: server-side uniqueness stops two sandboxes sharing a
// name, but not a name that spells another sandbox's ID. Such a name claims
// both of that sandbox's ID patterns at once, since it yields the bare and
// suffixed spelling of them.
func TestSSHConfigDropsANameThatSpellsAnotherSandboxesPattern(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress: sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{
			{id: "sbx_target00000001", name: "target"},
			// A legal, unique name that happens to spell the other one's ID.
			{id: "sbx_impostor000001", name: "sbx_target00000001"},
		},
	}
	out, _, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	// The contested spellings belong to neither, so they can never resolve to
	// the wrong sandbox.
	for _, contested := range []string{"Host sbx_target00000001 ", "sbx_target00000001.discobox.internal"} {
		if strings.Contains(out, contested) {
			t.Fatalf("contested pattern %q was emitted:\n%s", contested, out)
		}
	}
	// Each sandbox keeps the patterns nobody else claimed, so both stay
	// reachable: the target by its name, the impostor by its own ID.
	if !strings.Contains(out, "Host target target.discobox.internal\n") {
		t.Fatalf("the target lost the patterns it still owns:\n%s", out)
	}
	if !strings.Contains(out, "Host sbx_impostor000001 sbx_impostor000001.discobox.internal\n") {
		t.Fatalf("the impostor lost its own ID patterns:\n%s", out)
	}
}

// TestSSHConfigSkipsASandboxWithNoUnambiguousPattern: `Host` with no patterns
// is an ssh_config syntax error that would break the user's whole file, so a
// sandbox whose every spelling is contested is commented out instead. It takes
// a chain to get here — one sandbox named after the middle one's ID, and the
// middle one named after a third's ID — which is exactly why it is worth
// handling rather than assuming it cannot happen.
func TestSSHConfigSkipsASandboxWithNoUnambiguousPattern(t *testing.T) {
	fake := &sshConfigFakeServer{
		ingress: sshConfigEnabledIngress,
		sandboxes: []sshConfigFakeSandbox{
			{id: "sbx_third000000001", name: "third"},
			// Its name claims third's ID patterns; its own ID patterns are
			// claimed by the sandbox below.
			{id: "sbx_middle00000001", name: "sbx_third000000001"},
			{id: "sbx_first000000001", name: "sbx_middle00000001"},
		},
	}
	out, _, err := runSSHConfig(t, fake)
	if err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	if !strings.Contains(out, "# sbx_third000000001 (sbx_middle00000001) has no unambiguous host alias and was skipped") {
		t.Fatalf("the fully-contested sandbox should be reported, not emitted:\n%s", out)
	}
	for _, broken := range []string{"Host \n", "Host\n"} {
		if strings.Contains(out, broken) {
			t.Fatalf("emitted a Host line with no patterns, which breaks ssh_config:\n%s", out)
		}
	}
}
