package sshd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/services"
)

// fakeSandboxService implements services.SandboxService with only
// AcquireSandboxHTTPClient functional, recording the principal and scopes it
// was called with. Every other method panics: the handshake tests never
// reach them.
type fakeSandboxService struct {
	acquireCalls []acquireCall
	acquireErr   error
	// acquireResult, when set, is returned instead of acquireErr — for tests
	// that need a real lease pointed at a fake sandbox-agent/pool-agent HTTP
	// server.
	acquireResult func() (*services.HTTPClientLease, *model.Sandbox, error)
}

type acquireCall struct {
	principal auth.Principal
	ok        bool
	projectID string
	sandboxID string
	scopes    []string
}

func (f *fakeSandboxService) AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	f.acquireCalls = append(f.acquireCalls, acquireCall{principal: principal, ok: ok, projectID: projectID, sandboxID: sandboxID, scopes: scopes})
	if f.acquireResult != nil {
		return f.acquireResult()
	}
	if f.acquireErr != nil {
		return nil, nil, f.acquireErr
	}
	return nil, nil, errors.New("fakeSandboxService: no lease configured")
}

func (f *fakeSandboxService) DefaultSandboxImage() services.SandboxImageTarget {
	return services.SandboxImageTarget{}
}
func (f *fakeSandboxService) ListSandboxes(context.Context, string, string, string) ([]model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) CreateSandbox(context.Context, string, services.CreateSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) GetSandbox(context.Context, string, string) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) UpdateSandbox(context.Context, string, string, services.UpdateSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) DeleteSandbox(context.Context, string, string) error {
	panic("not implemented")
}
func (f *fakeSandboxService) StartSandbox(context.Context, string, string, services.StartSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) StopSandbox(context.Context, string, string, services.StopSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) RestartSandbox(context.Context, string, string, services.RestartSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) UpgradeSandbox(context.Context, string, string, services.UpgradeSandboxBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) CompleteSandboxSourcePush(context.Context, string, string, services.CompleteSandboxSourcePushBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) CompleteSandboxApply(context.Context, string, string, services.CompleteSandboxApplyBody) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) ReconcileSandbox(context.Context, string, string) (*model.Sandbox, error) {
	panic("not implemented")
}
func (f *fakeSandboxService) AssignSandboxHarnessSecrets(context.Context, string, string, string) (map[string]string, error) {
	panic("not implemented")
}

// testHarness wires a Server against an in-memory store fixture and a
// fakeSandboxService, and returns a client-side ssh.ClientConfig plus the
// listener address once Serve is running.
type testHarness struct {
	t         *testing.T
	server    *Server
	sandboxes *fakeSandboxService
	addr      string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()
	dataDir := t.TempDir()

	hostKey, err := LoadOrCreateHostKey(dataDir)
	if err != nil {
		t.Fatalf("host key: %v", err)
	}
	db := newRouteTestStore(t)
	sandboxes := &fakeSandboxService{acquireErr: errors.New("stub: attach not exercised by this test")}

	srv, err := NewServer(Options{
		HostKey:       hostKey,
		DataDir:       dataDir,
		Store:         db,
		Sandboxes:     sandboxes,
		DefaultUserID: "user_default",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ln, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Serve(ctx, ln) }()
	t.Cleanup(func() { _ = ln.Close() })

	return &testHarness{t: t, server: srv, sandboxes: sandboxes, addr: ln.Addr().String()}
}

func (h *testHarness) dial(user string, signer ssh.Signer) (*ssh.Client, error) {
	config := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test dials our own freshly generated local server; nothing to verify against
		Timeout:         5 * time.Second,
	}
	return ssh.Dial("tcp", h.addr, config)
}

func newTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	dir := t.TempDir()
	key, err := LoadOrCreateHostKey(dir) // reuses the ed25519 keypair generator
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	return key
}

func writeAuthorizedKeys(t *testing.T, dataDir string, signers ...ssh.Signer) {
	t.Helper()
	var content string
	for _, s := range signers {
		content += string(ssh.MarshalAuthorizedKey(s.PublicKey()))
	}
	if err := os.WriteFile(filepath.Join(dataDir, authorizedKeysFileName), []byte(content), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}
}

func TestHandshakeFileKeyAuthenticatesAsDefaultUserWithFullScope(t *testing.T) {
	h := newTestHarness(t)
	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)

	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme", "acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")

	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// Trigger a session channel so AcquireSandboxHTTPClient runs and we can
	// inspect the Principal it saw.
	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	_ = session.Shell() // the fake service errors; only the principal matters here

	waitForCalls(t, h.sandboxes, 1)
	call := h.sandboxes.acquireCalls[0]
	if !call.ok {
		t.Fatalf("expected a principal in context")
	}
	if call.principal.Type != auth.PrincipalTypeUser || call.principal.UserID != "user_default" {
		t.Fatalf("principal = %+v, want default user", call.principal)
	}
	if len(call.principal.Scopes) != 1 || call.principal.Scopes[0] != auth.ScopeAll {
		t.Fatalf("scopes = %v, want [%s]", call.principal.Scopes, auth.ScopeAll)
	}
	if call.projectID != acme || call.sandboxID != sandbox.ID {
		t.Fatalf("acquired for (%q, %q), want (%q, %q)", call.projectID, call.sandboxID, acme, sandbox.ID)
	}
}

func TestHandshakeProjectKeyGetsScopedBundle(t *testing.T) {
	h := newTestHarness(t)
	signer := newTestSigner(t)

	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme", "acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")

	pub := signer.PublicKey()
	key := &model.SSHKey{
		ProjectID:   acme,
		PublicKey:   string(ssh.MarshalAuthorizedKey(pub)),
		Fingerprint: ssh.FingerprintSHA256(pub),
		CreatedBy:   "user_enrolled",
	}
	if err := h.server.store.CreateSSHKey(context.Background(), key); err != nil {
		t.Fatalf("create ssh key: %v", err)
	}

	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()
	_ = session.Shell()

	waitForCalls(t, h.sandboxes, 1)
	call := h.sandboxes.acquireCalls[0]
	if call.principal.UserID != "user_enrolled" {
		t.Fatalf("UserID = %q, want the enrolling user", call.principal.UserID)
	}
	wantScopes := map[string]bool{"exec:read": true, "exec:write": true, "tcp:connect": true}
	if len(call.principal.Scopes) != len(wantScopes) {
		t.Fatalf("scopes = %v, want exactly %v", call.principal.Scopes, wantScopes)
	}
	for _, s := range call.principal.Scopes {
		if !wantScopes[s] {
			t.Fatalf("unexpected scope %q in %v", s, call.principal.Scopes)
		}
	}
}

func TestHandshakeUnknownKeyRejected(t *testing.T) {
	h := newTestHarness(t)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme", "acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")

	stranger := newTestSigner(t)
	if _, err := h.dial(sandbox.ID, stranger); err == nil {
		t.Fatalf("expected an unenrolled key to be rejected")
	}
}

func TestGlobalRequestsAreRejected(t *testing.T) {
	h := newTestHarness(t)
	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme", "acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")

	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	// tcpip-forward is the remote-forwarding request ADR 0024 §8 keeps out of
	// scope; it must come back refused, not merely unanswered.
	ok, _, err := client.SendRequest("tcpip-forward", true, nil)
	if err != nil {
		t.Fatalf("send global request: %v", err)
	}
	if ok {
		t.Fatalf("expected tcpip-forward to be refused")
	}
}

func TestUnknownChannelTypeRejected(t *testing.T) {
	h := newTestHarness(t)
	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme", "acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")

	client, err := h.dial(sandbox.ID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if _, _, err := client.OpenChannel("bogus-channel-type", nil); err == nil {
		t.Fatalf("expected an unknown channel type to be rejected")
	}
}

func waitForCalls(t *testing.T, svc *fakeSandboxService, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(svc.acquireCalls) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d AcquireSandboxHTTPClient call(s), got %d", n, len(svc.acquireCalls))
}
