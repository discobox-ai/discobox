//go:build unix

package handover

import (
	"context"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
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
