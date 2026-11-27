//go:build unix

package handover

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const listenersChildEnv = "DISCOBOX_HANDOVER_LISTENERS_CHILD"

// A process is handed the sockets systemd passed it by name, as sd_listen_fds
// passes them, and its children are not.
func TestListenersAreTheSocketsSystemdPassedByName(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("systemd passes sockets on Linux")
	}
	if os.Getenv(listenersChildEnv) != "" {
		listenersChild()
		return
	}
	var files []*os.File
	var addrs []string
	for range 2 {
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		file, err := listener.(*net.TCPListener).File()
		if err != nil {
			t.Fatal(err)
		}
		_ = listener.Close()
		files = append(files, file)
		addrs = append(addrs, listener.Addr().String())
	}
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestListenersAreTheSocketsSystemdPassedByName$") //nolint:gosec // G204: this test binary
	cmd.ExtraFiles = files
	cmd.Env = append(os.Environ(), listenersChildEnv+"=1", envListenFDs+"=2", envListenFDNames+"=ingress:proxy")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	for i, name := range []string{"ingress", "proxy"} {
		if !strings.Contains(string(out), name+"="+addrs[i]) {
			t.Errorf("the child was not handed %s at %s:\n%s", name, addrs[i], out)
		}
	}
	if !strings.Contains(string(out), "cleared") {
		t.Errorf("the child left the environment naming its sockets set:\n%s", out)
	}
}

// listenersChild is the process systemd starts: it claims LISTEN_PID, as
// systemd sets it for the process it execs, and reports what it was handed.
func listenersChild() {
	_ = os.Setenv(envListenPID, strconv.Itoa(os.Getpid()))
	listeners, err := Listeners()
	if err != nil {
		os.Stdout.WriteString("error " + err.Error() + "\n")
		os.Exit(1)
	}
	for name, listener := range listeners {
		os.Stdout.WriteString(name + "=" + listener.Addr().String() + "\n")
	}
	if os.Getenv(envListenFDs) == "" && os.Getenv(envListenPID) == "" {
		os.Stdout.WriteString("cleared\n")
	}
	os.Exit(0)
}

// A process systemd passed nothing to is handed nothing.
func TestListenersAreNoneWhenNothingWasPassed(t *testing.T) {
	t.Setenv(envListenPID, strconv.Itoa(os.Getpid()+1))
	t.Setenv(envListenFDs, "2")
	listeners, err := Listeners()
	if err != nil || len(listeners) != 0 {
		t.Fatalf("Listeners for another process's sockets = %v, %v; want none", listeners, err)
	}
}

// A drain asked for while its mark cannot be taken is refused, and the
// process serves on; one asked for once it can is taken, with the mark held.
func TestADrainIsTakenOnlyWithItsMarkHeld(t *testing.T) {
	dir := t.TempDir()
	markPath := filepath.Join(dir, "agent.draining")
	// A directory where the mark's file goes: it cannot be taken.
	if err := os.Mkdir(markPath, 0o700); err != nil {
		t.Fatal(err)
	}
	refused := make(chan error, 4)
	drain := DrainRequested(t.Context(), markPath, func(err error) { refused <- err })
	defer drain.Release()
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-refused:
	case <-time.After(5 * time.Second):
		t.Fatal("a drain whose mark cannot be taken was not refused")
	}
	select {
	case <-drain.Requested():
		t.Fatal("a drain was taken without its mark")
	default:
	}

	if err := os.Remove(markPath); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	select {
	case <-drain.Requested():
	case <-time.After(5 * time.Second):
		t.Fatal("a drain asked for once its mark could be taken was not taken")
	}
	if held, err := Held(markPath); err != nil || !held {
		t.Fatalf("the drain's mark held = %v, %v; want held", held, err)
	}
	// A later signal is ignored: left to its default action, it would end
	// this process, and the drain with it.
	if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
}
