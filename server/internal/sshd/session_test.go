package sshd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/transport"
)

// fakeExecRecordJSON is a minimal but schema-valid SandboxExec: the generated
// decoder rejects a response missing any required field.
const fakeExecRecordJSON = `{"id":"ex_test0000000000","command":["/bin/bash"],"createdAt":"2026-08-06T00:00:00Z",` +
	`"status":"starting","tty":false,"workdir":"/home/user"}`

// fakeExecAgent stands in for the sandbox-agent's exec endpoints, reproducing
// the one behavior the SSH session path depends on: an exec is created
// suspended and produces nothing until it is started. Output is written by the
// start handler, so a session that never starts its exec hangs here exactly as
// it does against a real sandbox.
type fakeExecAgent struct {
	stdout   string
	exitCode int64
	// failCreate makes POST /execs fail, standing in for every reason a
	// sandbox cannot start a session.
	failCreate bool

	mu             sync.Mutex
	createBody     map[string]any
	attachOpened   bool
	startCalled    bool
	startedAfterAt bool

	attached chan struct{} // closed when the attach websocket is open
	release  chan struct{} // closed by the start handler
}

func newFakeExecAgent(stdout string, exitCode int64) *fakeExecAgent {
	return &fakeExecAgent{
		stdout:   stdout,
		exitCode: exitCode,
		attached: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (f *fakeExecAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/execs"):
		if f.failCreate {
			http.Error(w, "sandbox is not accepting execs", http.StatusServiceUnavailable)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.createBody = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"exec":` + fakeExecRecordJSON + `}`))

	case strings.HasSuffix(r.URL.Path, "/attach"):
		f.serveAttach(w, r)

	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/start"):
		f.mu.Lock()
		f.startCalled = true
		f.startedAfterAt = f.attachOpened
		f.mu.Unlock()
		close(f.release)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fakeExecRecordJSON))

	default:
		http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
	}
}

func (f *fakeExecAgent) serveAttach(w http.ResponseWriter, r *http.Request) {
	// Recorded before the handshake, not after: Accept writes the 101 that
	// releases the client, so anything set afterwards races the client's next
	// request. The arrival of this request is the event the ordering assertion
	// is about, and it strictly precedes any start the client could send.
	f.mu.Lock()
	f.attachOpened = true
	f.mu.Unlock()

	wsConn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	netConn := websocket.NetConn(r.Context(), wsConn, websocket.MessageBinary)
	defer netConn.Close()

	close(f.attached)

	// Nothing is emitted until the exec is started, which is what makes this
	// test fail — rather than pass with a race — if start is never called.
	select {
	case <-f.release:
	case <-r.Context().Done():
		return
	}

	if err := frame.Write(netConn, frame.Stdout, []byte(f.stdout)); err != nil {
		return
	}
	exitPayload, err := frame.EncodeExit("exited", &f.exitCode, "")
	if err != nil {
		return
	}
	_ = frame.Write(netConn, frame.Exit, exitPayload)
}

// newExecSessionHarness wires a test SSH server whose leases point at agent.
func newExecSessionHarness(t *testing.T, agent *fakeExecAgent) (*testHarness, string) {
	t.Helper()
	agentServer := httptest.NewServer(agent)
	t.Cleanup(agentServer.Close)

	h := newTestHarness(t)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")
	h.sandboxes.acquireResult = func() (*services.HTTPClientLease, *model.Sandbox, error) {
		lease := &transport.HTTPClientLease{Client: agentServer.Client(), BaseURL: agentServer.URL}
		return lease, &model.Sandbox{ID: sandbox.ID, ProjectID: acme, PoolID: "pool-1"}, nil
	}
	return h, sandbox.ID
}

// TestSessionExecStartsTheExecAndStreamsOutput is the end-to-end shape of an
// `ssh host command`: the session channel must create, attach, and *start* the
// exec, then deliver its output and exit status. Without the start call the
// exec stays suspended and the session hangs forever.
func TestSessionExecStartsTheExecAndStreamsOutput(t *testing.T) {
	agent := newFakeExecAgent("hello from the sandbox\n", 7)
	h, sandboxID := newExecSessionHarness(t, agent)

	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)

	client, err := h.dial(sandboxID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := session.Output("echo hello")
		done <- result{out: out, err: err}
	}()

	var got result
	select {
	case got = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ssh exec never completed: the exec was created but never started")
	}

	if string(got.out) != "hello from the sandbox\n" {
		t.Fatalf("stdout = %q, want %q", got.out, "hello from the sandbox\n")
	}
	var exitErr *ssh.ExitError
	if !errors.As(got.err, &exitErr) {
		t.Fatalf("err = %v (%T), want *ssh.ExitError carrying the exit status", got.err, got.err)
	}
	if exitErr.ExitStatus() != 7 {
		t.Fatalf("exit status = %d, want 7", exitErr.ExitStatus())
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !agent.startCalled {
		t.Fatal("the exec was never started")
	}
	if !agent.startedAfterAt {
		// A fast command broadcasts its output as it exits, so starting before
		// the attach websocket is open races the exec to its own output.
		t.Fatal("the exec was started before its attach websocket was open")
	}
	if cmdLine, _ := agent.createBody["shellCommandLine"].(string); cmdLine != "echo hello" {
		t.Fatalf("shellCommandLine = %q, want %q", cmdLine, "echo hello")
	}
	assertHomeWorkdir(t, agent.createBody)
}

// assertHomeWorkdir pins the workdir every session channel must request. SSH
// starts a session in the user's home directory; the sandbox's own exec
// default is the primary source directory, so inheriting it would put `scp
// file host:` uploads inside the sandbox's git working tree.
func assertHomeWorkdir(t *testing.T, createBody map[string]any) {
	t.Helper()
	workdir, _ := createBody["workdir"].(string)
	if workdir != sessionHomeWorkdir {
		t.Fatalf("workdir = %q, want %q (the run user's home directory)", workdir, sessionHomeWorkdir)
	}
}

// TestSessionSubsystemSFTPStartsTheExec covers the other dispatch that reaches
// createAndDialExec: an sftp subsystem is an exec too, and is equally dead if
// it is never started.
func TestSessionSubsystemSFTPStartsTheExec(t *testing.T) {
	agent := newFakeExecAgent("", 0)
	h, sandboxID := newExecSessionHarness(t, agent)

	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)

	client, err := h.dial(sandboxID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	if err := session.RequestSubsystem("sftp"); err != nil {
		t.Fatalf("request sftp subsystem: %v", err)
	}

	select {
	case <-agent.release:
	case <-time.After(15 * time.Second):
		t.Fatal("the sftp exec was never started")
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	command, _ := agent.createBody["command"].([]any)
	if len(command) != 1 || command[0] != sftpServerPath {
		t.Fatalf("command = %v, want [%s]", command, sftpServerPath)
	}
	if tty, _ := agent.createBody["tty"].(bool); tty {
		t.Fatal("the sftp subsystem must never allocate a pty")
	}
	// sftp is where the wrong default bites hardest: a client that uploads to
	// a bare relative path lands wherever the exec started.
	assertHomeWorkdir(t, agent.createBody)
}

// TestSessionFailureReachesTheClient is the difference between "shell request
// failed on channel 0" and knowing why. SSH_MSG_CHANNEL_FAILURE carries no
// message and OpenSSH discards extended data on a refused request, so the
// reason arrives as an ordinary session: stderr, plus a non-zero exit.
func TestSessionFailureReachesTheClient(t *testing.T) {
	agent := newFakeExecAgent("", 0)
	agent.failCreate = true
	h, sandboxID := newExecSessionHarness(t, agent)

	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)

	client, err := h.dial(sandboxID, signer)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer session.Close()

	var stderr strings.Builder
	session.Stderr = &stderr
	if err := session.Shell(); err != nil {
		t.Fatalf("the request is accepted so the reason can be delivered: %v", err)
	}
	err = session.Wait()
	var exitErr *ssh.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("wait = %v (%T), want a non-zero exit carrying the failure", err, err)
	}
	if exitErr.ExitStatus() != sessionSetupExitStatus {
		t.Fatalf("exit status = %d, want %d for a session that never ran", exitErr.ExitStatus(), sessionSetupExitStatus)
	}
	if !strings.Contains(stderr.String(), "could not start a session") {
		t.Fatalf("stderr should carry the reason, got %q", stderr.String())
	}
	// The underlying cause, not just that there was one.
	if !strings.Contains(stderr.String(), "sandbox is not accepting execs") {
		t.Fatalf("stderr should carry the sandbox's own error, got %q", stderr.String())
	}
}

// TestSessionAuthorizationFailureSaysNothingSpecific: whether this connection
// may reach this sandbox is the one answer that must not distinguish "no such
// sandbox" from "not yours" (ADR 0024 §1), so both statuses collapse into one
// message with nothing of the original in it.
func TestSessionAuthorizationFailureSaysNothingSpecific(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		h := newTestHarness(t)
		acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme")
		sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")
		h.sandboxes.acquireErr = apperrors.NewStatusError(status,
			"sandbox sbx_secret000000001 not found in project proj_secret00000001")

		signer := newTestSigner(t)
		writeAuthorizedKeys(t, h.server.dataDir, signer)
		client, err := h.dial(sandbox.ID, signer)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		session, err := client.NewSession()
		if err != nil {
			t.Fatalf("new session: %v", err)
		}
		var stderr strings.Builder
		session.Stderr = &stderr
		if err := session.Shell(); err != nil {
			t.Fatalf("shell: %v", err)
		}
		_ = session.Wait()
		_ = session.Close()
		_ = client.Close()

		if !strings.Contains(stderr.String(), "not authorized") {
			t.Fatalf("status %d: stderr = %q, want the generic message", status, stderr.String())
		}
		for _, leaked := range []string{"sbx_secret", "proj_secret", "not found"} {
			if strings.Contains(stderr.String(), leaked) {
				t.Fatalf("status %d: stderr leaked %q: %s", status, leaked, stderr.String())
			}
		}
	}
}

// TestSessionOperationalFailureSaysWhat: a pool that is not active or a sandbox
// that cannot start is not an authorization problem, and reporting it as one
// sends the reader to look at keys and grants for something that is neither.
func TestSessionOperationalFailureSaysWhat(t *testing.T) {
	h := newTestHarness(t)
	acme := createRouteFixtureProject(t, h.server.store, "proj_acme00000000", "Acme")
	sandbox := createRouteFixtureSandbox(t, h.server.store, acme, "devbox")
	h.sandboxes.acquireErr = apperrors.NewStatusError(http.StatusConflict, "sandbox pool is not active")

	signer := newTestSigner(t)
	writeAuthorizedKeys(t, h.server.dataDir, signer)
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

	var stderr strings.Builder
	session.Stderr = &stderr
	if err := session.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}
	_ = session.Wait()
	if !strings.Contains(stderr.String(), "sandbox pool is not active") {
		t.Fatalf("stderr = %q, want the operational cause", stderr.String())
	}
	if strings.Contains(stderr.String(), "not authorized") {
		t.Fatalf("an operational failure must not be reported as an authorization one: %s", stderr.String())
	}
}
