//go:build windows

package endpoint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// consoleReportPathEnv names the file the child below writes its report to. It
// is also what tells the child it is a child: the helper skips without it.
const consoleReportPathEnv = "DISCOBOX_TEST_CONSOLE_REPORT"

// TestBackgroundServerConsoleHelper is not a test of anything. It is the
// process the test below launches, and it reports the console it was given.
func TestBackgroundServerConsoleHelper(t *testing.T) {
	path := os.Getenv(consoleReportPathEnv)
	if path == "" {
		t.Skip("not the console-report child")
	}
	report := fmt.Sprintf("console=%t window=%d", attachedToAConsole(), consoleWindow())
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A server launched in the background must end up with a console that has no
// window — not with no console at all.
//
// The difference is only visible in what the server starts. Windows gives a
// process with no console a fresh one, window and all, for every console
// program it runs, and the server runs them without meaning to: a registry
// pull resolves credentials through whichever docker-credential-*.exe the
// user's Docker config names. Setting up the first pool inspects one harness
// image per built-in harness, and each of those put a console window on the
// user's desktop for as long as the helper ran.
func TestBackgroundServerGetsAConsoleWithNoWindow(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(t.TempDir(), "console.txt")
	cmd := exec.CommandContext(t.Context(), self, "-test.run=^TestBackgroundServerConsoleHelper$")
	cmd.Env = append(os.Environ(), consoleReportPathEnv+"="+reportPath)
	setDetachedProcess(cmd)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run the console-report child: %v\n%s", err, output)
	}

	report, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(string(report))
	// console=false is DETACHED_PROCESS, which also makes CREATE_NO_WINDOW
	// ignored if both are set. A non-zero window is a console window on screen.
	if want := "console=true window=0"; got != want {
		t.Fatalf("the launched process reports %q, want %q", got, want)
	}
}

// attachedToAConsole reports whether this process has a console at all.
// GetConsoleCP fails for a process that has none, which is what tells a
// windowless console apart from no console.
func attachedToAConsole() bool {
	_, err := windows.GetConsoleCP()
	return err == nil
}

// consoleWindow is the handle of this process's console window, or zero when
// the console has none. It is not in x/sys/windows.
func consoleWindow() uintptr {
	handle, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	return handle
}

// A named pipe whose server is not accepting — every instance busy, as on a
// loaded host — makes go-winio's dial retry until the probe times out. That
// is a server too busy to answer, not an absent one. A pipe nobody has
// created is absent, and says so at once, so a cold start waits for nothing.
func TestProbeTellsABusyPipeFromAMissingOne(t *testing.T) {
	pipe := fmt.Sprintf(`\\.\pipe\discobox-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	opts := LaunchOptions{Endpoint: "npipe:" + strings.ReplaceAll(pipe, `\`, "/"), ProbeTimeout: 200 * time.Millisecond}

	start := time.Now()
	_, err := probeEndpoint(context.Background(), opts)
	if err == nil || errors.Is(err, errProbeUnanswered) || !isProbeConnectionError(err) {
		t.Fatalf("probe of a missing pipe = %v, want no server", err)
	}
	if elapsed := time.Since(start); elapsed >= opts.ProbeTimeout {
		t.Fatalf("probe of a missing pipe took %s, want it reported at once", elapsed)
	}

	// Listening, and never accepting: go-winio keeps no instance open for a
	// client until Accept is called, so every dial finds the pipe busy.
	listener, err := winio.ListenPipe(pipe, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, err = probeEndpoint(context.Background(), opts)
	if !errors.Is(err, errProbeUnanswered) || isProbeConnectionError(err) {
		t.Fatalf("probe of a busy pipe = %v, want a server too busy to answer", err)
	}
}

// A loopback TCP port nothing listens on is refused on Windows only after
// about two seconds, past the default probe timeout. It must still read as no
// server, or every cold start on an http:// endpoint would wait out
// ReadyTimeout on a server that is not there.
func TestProbeReadsASlowRefusalAsNoServer(t *testing.T) {
	reserved, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reserved.Addr().String()
	_ = reserved.Close()
	_, err = probeEndpoint(context.Background(), LaunchOptions{Endpoint: "http://" + addr})
	if err == nil || errors.Is(err, errProbeUnanswered) || !isProbeConnectionError(err) {
		t.Fatalf("probe of a closed port = %v, want no server", err)
	}
}
