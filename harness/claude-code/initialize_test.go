package claudecode

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInitializationWaitsForCredentialHandoffLock(t *testing.T) {
	for _, bin := range []string{"node", "flock", "sh"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	home := t.TempDir()
	dir := filepath.Join(home, ".config", "discobox")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(home, ".claude.json")
	const initial = `{"primaryApiKey":"newer-local-fixture"}`
	if err := os.WriteFile(state, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	hold := exec.CommandContext(t.Context(), "flock", "-x", filepath.Join(dir, "harness-capture.lock"), "sh", "-c", "echo locked; cat >/dev/null") //nolint:gosec // Fixed test command and an isolated temporary lock path.
	input, err := hold.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := hold.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := hold.Start(); err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "locked\n" {
		t.Fatalf("lock holder: %q %v", ready, err)
	}
	wrapper, err := os.ReadFile("capture.sh")
	if err != nil {
		t.Fatal(err)
	}
	runtimePath, err := filepath.Abs("../capture/runtime.cjs")
	if err != nil {
		t.Fatal(err)
	}
	metadataPath, err := filepath.Abs("image.json")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(wrapper), "/usr/local/libexec/discobox/harness-capture/runtime.cjs", runtimePath)
	script = strings.ReplaceAll(script, `$(dirname "$0")/claude-code-image.json`, metadataPath)
	cmd := exec.CommandContext(t.Context(), "sh", "-c", script, "capture", "init")
	cmd.Env = append(os.Environ(), "HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("initializer did not wait for lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	raw, err := os.ReadFile(state)
	if err != nil || string(raw) != initial {
		t.Fatal("state changed while handoff lock held")
	}
	if err := input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := hold.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(state)
	if err != nil || !strings.Contains(string(raw), `"hasCompletedOnboarding": true`) {
		t.Fatal("initialization did not run after handoff released the lock")
	}
}
