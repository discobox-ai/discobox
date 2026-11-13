package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/coder/websocket"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/sandbox-agent/server"
)

// TestMain lets this test binary be the agent: run with the agent's own
// arguments — `--config`, or a subcommand such as the exec-shim its
// supervisor starts from its own executable — it is the agent, exactly as
// the binary a launch daemon runs.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(run("discobox-sandbox-agent", os.Args[1:]))
	}
	os.Exit(m.Run())
}

const (
	bootProject = "proj_darwin0000001"
	bootSandbox = "sbx_darwin00000001"
)

// bootTestEnv turns the boot test on. It needs what a macOS template gives
// its guest and a Mac does not have: /run, made through synthetic.conf, and
// /run/discobox and /var/lib/discobox writable by the user the test runs as.
// scripts/darwin-sandbox-dirs.sh makes them; ci:test:darwin runs it and sets
// this.
const bootTestEnv = "DISCOBOX_DARWIN_BOOT_TEST"

// The darwin agent as a launch daemon starts it (ADR 26-10-09-143 §1): before
// its bootstrap is there, at the paths the pool's bootstrap names. It waits,
// and once sandbox.json is placed by a rename it serves the API, runs an
// exec, and attaches a terminal.
func TestTheAgentBootsServesAndAttachesOnDarwin(t *testing.T) {
	if os.Getenv(bootTestEnv) == "" {
		t.Skipf("set %s on a Mac prepared by scripts/darwin-sandbox-dirs.sh", bootTestEnv)
	}
	// The launch daemon's pre-start steps (agentlayer.LaunchdUnits), but for
	// /workspace: the working root here is the test's own.
	if err := os.MkdirAll(poolTerminalsDir, 0o700); err != nil {
		t.Fatalf("the template's /run is not there: %v", err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "etc", "sandbox.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	publicKey, sign := bootSigner(t)
	addr := freeAddr(t)
	base := "http://" + addr

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// --drop-to-account, as the launch daemon passes it: unprivileged here, the
	// agent already is the bootstrap's account, and keeps running as it.
	agent := exec.CommandContext(ctx, os.Args[0], "--config", configPath, "--"+dropToAccountFlag) //nolint:gosec // G204: this test binary, run as the agent.
	var logs syncBuffer
	agent.Stdout, agent.Stderr = &logs, &logs
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- agent.Wait() }()
	defer func() {
		_ = agent.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(20 * time.Second):
			_ = agent.Process.Kill()
		}
		if t.Failed() {
			t.Logf("agent log:\n%s", logs.String())
		}
	}()

	// Started first, as launchd starts it: it waits rather than exiting or
	// serving.
	waitFor(t, 30*time.Second, "the agent waiting for its bootstrap", func() bool {
		return strings.Contains(logs.String(), "waiting for a file to appear")
	})
	select {
	case err := <-exited:
		t.Fatalf("the agent exited before its bootstrap was placed: %v", err)
	default:
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := get(ctx, client, base+"/healthz"); err == nil {
		resp.Body.Close()
		t.Fatal("the agent serves before its bootstrap is placed")
	}

	placeBootstrap(t, configPath, dir, addr, publicKey)
	waitFor(t, time.Minute, "the agent to serve", func() bool {
		resp, err := get(ctx, client, base+"/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	token := sign(server.ScopeExecRead, server.ScopeExecWrite)
	execs := base + "/api/projects/" + bootProject + "/sandboxes/" + bootSandbox + "/execs"

	// An exec, run to completion.
	id := createExec(ctx, t, client, execs, token, map[string]any{"command": []string{"/bin/sh", "-c", "echo out-$((40+2)); uname -s"}})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, execs+"/"+id+"/attach", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(out), "out-42") || !strings.Contains(string(out), "Darwin") {
		t.Fatalf("one-shot exec = %d %q, want its output", resp.StatusCode, out)
	}

	// A terminal: a shell on a PTY, attached over the websocket.
	id = createExec(ctx, t, client, execs, token, map[string]any{"command": []string{"/bin/sh"}, "tty": true, "cols": 80, "rows": 24})
	start, _ := http.NewRequestWithContext(ctx, http.MethodPost, execs+"/"+id+"/start", nil)
	start.Header.Set("Authorization", "Bearer "+token)
	if resp, err := client.Do(start); err != nil {
		t.Fatal(err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			t.Fatalf("start = %d %s", resp.StatusCode, body)
		}
	}
	ws, upgrade, err := websocket.Dial(ctx, "ws://"+addr+strings.TrimPrefix(execs, base)+"/"+id+"/attach", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if upgrade != nil && upgrade.Body != nil {
		upgrade.Body.Close()
	}
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()
	if err := frame.Write(conn, frame.Ready, nil); err != nil {
		t.Fatal(err)
	}
	if err := frame.Write(conn, frame.Input, []byte("test -t 0 && echo tty-$((2+3)); exit 7\n")); err != nil {
		t.Fatal(err)
	}
	var screen strings.Builder
	for {
		f, err := frame.Read(conn)
		if err != nil {
			t.Fatalf("read the terminal: %v; it showed %q", err, screen.String())
		}
		if f.Type == frame.Stdout || f.Type == frame.Stderr {
			screen.Write(f.Payload)
			continue
		}
		if f.Type == frame.Exit {
			exit, err := frame.DecodeExit(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if exit.ExitCode == nil || *exit.ExitCode != 7 {
				t.Fatalf("terminal exit = %+v, want 7", exit)
			}
			break
		}
	}
	if !strings.Contains(screen.String(), "tty-5") {
		t.Fatalf("the terminal showed %q, want a shell on a TTY", screen.String())
	}

	// The exec state is under the runtime directory the pool's bootstrap
	// names, on the template's /run: the paths do not differ by platform.
	if entries, err := os.ReadDir(poolTerminalsDir); err != nil || len(entries) == 0 {
		t.Fatalf("no exec state under %s: %v (%d entries)", poolTerminalsDir, err, len(entries))
	}
	// The primary terminal launched as the sandbox's one account: a launch
	// restating that account's home in its request is refused on darwin.
	if strings.Contains(logs.String(), "launch primary terminal") {
		t.Fatalf("the primary terminal did not launch")
	}
}

// poolTerminalsDir and poolDatabase are the agentRuntime paths the pool writes
// into every bootstrap (pool-agent/sandboxruntime's buildSandboxDocument).
const (
	poolTerminalsDir = "/run/discobox/agent-terminals"
	poolDatabase     = "/var/lib/discobox/sandbox-agent.db"
)

// placeBootstrap writes the sandbox's bootstrap as a backend places it: whole,
// by a rename, with the agent paths the pool's bootstrap names. The sandbox's
// account is this process's, which the agent runs as, and its working root the
// test's own: /workspace is the template's too, and one a test can make.
func placeBootstrap(t *testing.T, path, dir, addr, publicKey string) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"apiVersion": sandboxconfig.APIVersion,
		"sandboxId":  bootSandbox,
		"provider": map[string]any{
			"kind":       "discobox-pool",
			"projectId":  bootProject,
			"poolId":     "pool_darwin0000001",
			"publicKeys": map[string]string{sandboxconfig.ControlPlanePublicKeyName: publicKey},
		},
		// A darwin sandbox has one account, the agent's, named and nothing
		// more: no uid, gid or home (ADR 0145 §5).
		"user": map[string]any{"name": current.Username},
		"agentRuntime": map[string]any{
			"listenAddress": addr,
			"workingRoot":   work,
			"runtimeDir":    poolTerminalsDir,
			"databasePath":  poolDatabase,
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

func createExec(ctx context.Context, t *testing.T, client *http.Client, url, token string, body map[string]any) string {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var created struct {
		Exec struct {
			ID string `json:"id"`
		} `json:"exec"`
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(raw, &created) != nil || created.Exec.ID == "" {
		t.Fatalf("create exec = %d %s", resp.StatusCode, raw)
	}
	return created.Exec.ID
}

// bootSigner is the control plane's half of the agent's token check: the
// public key the bootstrap names, and tokens it signs for this sandbox.
func bootSigner(t *testing.T) (string, func(scopes ...string) string) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := paseto.NewV4AsymmetricSecretKeyFromEd25519(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(publicKey), func(scopes ...string) string {
		now := time.Now()
		token := paseto.NewToken()
		token.SetAudience(server.SandboxAgentAudience)
		token.SetIssuedAt(now)
		token.SetNotBefore(now.Add(-time.Minute))
		token.SetExpiration(now.Add(time.Hour))
		token.SetString("project_id", bootProject)
		token.SetString("sandbox_id", bootSandbox)
		if err := token.Set("scopes", scopes); err != nil {
			t.Fatal(err)
		}
		return token.V4Sign(secret, nil)
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func waitFor(t *testing.T, limit time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// syncBuffer is the agent's output, written by its process and read by the
// test at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
