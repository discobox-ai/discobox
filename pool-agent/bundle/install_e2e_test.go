package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	poolagent "github.com/discobox-ai/discobox/pool-agent"
)

const dockerIntegrationEnv = "DISCOBOX_DOCKER_INTEGRATION"

// proxyEnv is the proxy environment a nested container is handed where this
// test's own egress is through one.
var proxyEnv = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"}

// The issue's done-when (ADR 26-10-23-551 §1): on a plain Debian systemd
// machine, copying the bundle's tree and a bootstrap in and running install
// starts a pool agent; running it again changes nothing; a new tree and
// install restarts the agent onto it; and a reboot brings it back with no new
// registration.
func TestInstallOnASystemdMachineE2E(t *testing.T) {
	if os.Getenv(dockerIntegrationEnv) != "1" {
		t.Skipf("set %s=1 to run the pool-agent bundle e2e test", dockerIntegrationEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "tree")
	docker(ctx, t, "buildx", "build", "-f", filepath.Join(repoRoot, "pool-agent/Dockerfile"), "--target", "bundle",
		"--output", "type=local,dest="+tree, repoRoot)
	image := "discobox-pool-agent-bundle-machine:e2e"
	docker(ctx, t, "build", "-t", image, "testdata/machine")

	plane := startControlPlane(t)

	name := fmt.Sprintf("discobox-pool-agent-bundle-e2e-%d", time.Now().UnixNano())
	run := []string{"run", "-d", "--name", name, "--privileged", "--cgroupns=private",
		"--tmpfs", "/run", "--tmpfs", "/run/lock",
		"--add-host", "host.docker.internal:host-gateway"}
	// Where egress is through a proxy (a discobox), the machine is handed it as
	// any nested container is, for install's apt-get.
	for _, key := range proxyEnv {
		if os.Getenv(key) != "" {
			run = append(run, "-e", key)
		}
	}
	docker(ctx, t, append(run, image)...)
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := exec.CommandContext(context.Background(), "docker", "exec", name, "journalctl", "--no-pager", "-n", "200", "-u", AgentUnit, "-u", ProxyUnit).CombinedOutput()
			t.Logf("journal:\n%s", out)
		}
		_ = exec.CommandContext(context.Background(), "docker", "rm", "-f", name).Run()
	})
	waitFor(ctx, t, "systemd to boot", func() bool {
		out, _ := exec.CommandContext(ctx, "docker", "exec", name, "systemctl", "is-system-running").Output()
		state := strings.TrimSpace(string(out))
		return state == "running" || state == "degraded"
	})

	// install runs as the provider's exec does, with the machine's own
	// environment: here, whatever proxy it was handed.
	var installEnv []string
	for _, entry := range strings.Split(docker(ctx, t, "exec", name, "cat", "/proc/1/environ"), "\x00") {
		if key, _, ok := strings.Cut(entry, "="); ok && slices.Contains(proxyEnv, key) {
			installEnv = append(installEnv, "-e", entry)
		}
	}
	install := func(agent string) {
		docker(ctx, t, append(append([]string{"exec"}, installEnv...), name, agent, "install")...)
	}

	// What a provider places: the tree under its digest, and the bootstrap,
	// root's alone.
	place := func(digest string) string {
		dir := TreeDir(digest)
		docker(ctx, t, "exec", name, "mkdir", "-p", filepath.Dir(dir))
		docker(ctx, t, "cp", tree, name+":"+dir)
		return dir + "/" + AgentFile
	}
	bootstrap, err := json.Marshal(poolagent.Bootstrap{
		ControlPlaneURL: plane.url,
		ProjectID:       "project-e2e",
		PoolID:          "pool-e2e",
		Token:           "bootstrap-token-e2e",
		Runtime:         poolagent.RuntimeMachine,
		Driver:          "boxd",
	})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapFile := filepath.Join(t.TempDir(), "bootstrap.json")
	if err := os.WriteFile(bootstrapFile, bootstrap, 0o600); err != nil {
		t.Fatal(err)
	}
	docker(ctx, t, "exec", name, "mkdir", "-p", filepath.Dir(BootstrapFile))
	docker(ctx, t, "cp", bootstrapFile, name+":"+BootstrapFile)
	docker(ctx, t, "exec", name, "chown", "root:root", BootstrapFile)
	docker(ctx, t, "exec", name, "chmod", "0600", BootstrapFile)

	agentPID := func() string {
		return strings.TrimSpace(docker(ctx, t, "exec", name, "systemctl", "show", "-p", "MainPID", "--value", AgentUnit))
	}
	running := func(what string) string {
		waitFor(ctx, t, what, func() bool {
			out, _ := exec.CommandContext(ctx, "docker", "exec", name, "systemctl", "is-active", AgentUnit, ProxyUnit).Output()
			return strings.TrimSpace(string(out)) == "active\nactive"
		})
		plane.waitForStatus(ctx, t)
		return agentPID()
	}

	const digestOne = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const digestTwo = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

	// Install starts a pool agent, which registers once.
	install(place(digestOne))
	first := running("the agent and proxy to start")
	if got := plane.registrations(); got != 1 {
		t.Fatalf("registrations = %d after install, want 1", got)
	}
	if out := docker(ctx, t, "exec", name, "git", "--version"); !strings.HasPrefix(out, "git version") {
		t.Fatalf("git --version = %q, want git installed", out)
	}

	// Again, it changes nothing: the same agent keeps running.
	install(TreeDir(digestOne) + "/" + AgentFile)
	if again := running("the agent to keep running"); again != first {
		t.Fatalf("agent pid %s -> %s on a second install, want it untouched", first, again)
	}

	// A new tree and install restarts the agent onto it.
	install(place(digestTwo))
	if got := strings.TrimSpace(docker(ctx, t, "exec", name, "readlink", CurrentLink)); got != TreeDir(digestTwo) {
		t.Fatalf("%s -> %s, want %s", CurrentLink, got, TreeDir(digestTwo))
	}
	moved := running("the agent to restart onto the new tree")
	if moved == first {
		t.Fatalf("agent pid %s unchanged after installing a new tree", moved)
	}
	if out, err := exec.CommandContext(ctx, "docker", "exec", name, "test", "-e", TreeDir(digestOne)).CombinedOutput(); err == nil {
		t.Fatalf("the superseded tree %s is still on the machine: %s", TreeDir(digestOne), out)
	}

	// A reboot brings it back from the enabled units, spending no token.
	statuses := plane.statusCount()
	docker(ctx, t, "restart", name)
	waitFor(ctx, t, "status after the reboot", func() bool { return plane.statusCount() > statuses })
	running("the agent to come back after a reboot")
	if got := plane.registrations(); got != 1 {
		t.Fatalf("registrations = %d after a new tree and a reboot, want still 1", got)
	}
}

// controlPlane is enough of one for a pool agent to register and report to.
type controlPlane struct {
	url      string
	mu       sync.Mutex
	register int
	status   int
	statusCh chan struct{}
}

func startControlPlane(t *testing.T) *controlPlane {
	t.Helper()
	// The machine reaches it at host.docker.internal, the Docker bridge's
	// gateway, which a loopback listener does not answer on.
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "0.0.0.0:0") //nolint:gosec // G102: see above; a test's fake control plane, for the test's run.
	if err != nil {
		t.Fatal(err)
	}
	plane := &controlPlane{
		url:      fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port),
		statusCh: make(chan struct{}, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/pools/register", func(w http.ResponseWriter, _ *http.Request) {
		plane.mu.Lock()
		plane.register++
		plane.mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("POST /api/pools/{poolId}/status", func(w http.ResponseWriter, _ *http.Request) {
		plane.mu.Lock()
		plane.status++
		plane.mu.Unlock()
		select {
		case plane.statusCh <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return plane
}

func (p *controlPlane) registrations() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.register
}

func (p *controlPlane) statusCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// waitForStatus waits for a status report after this call.
func (p *controlPlane) waitForStatus(ctx context.Context, t *testing.T) {
	t.Helper()
	select {
	case <-p.statusCh:
	default:
	}
	select {
	case <-p.statusCh:
	case <-time.After(2 * time.Minute):
		t.Fatal("no status report from the agent")
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func docker(ctx context.Context, t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func waitFor(ctx context.Context, t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for !done() {
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
