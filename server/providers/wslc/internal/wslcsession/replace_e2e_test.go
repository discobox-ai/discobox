//go:build windows

package wslcsession

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const replaceE2EName = "discobox-wslcsession-replace-e2e"

// TestReplaceExistingHolderE2E is not a test on its own: TestReplaceExistingE2E
// re-executes the test binary to run it as a separate process, which boots a VM
// on the storage it is given and then waits to be killed.
func TestReplaceExistingHolderE2E(t *testing.T) {
	storage := os.Getenv("DISCOBOX_WSLC_REPLACE_HOLDER_STORAGE")
	if storage == "" {
		t.Skip("run by TestReplaceExistingE2E as its holder process")
	}
	session, err := NewSession(Options{DisplayName: replaceE2EName, StoragePath: storage, MaxStorageSizeMB: 8192})
	if err != nil {
		os.Stdout.WriteString("HOLDER-ERR " + err.Error() + "\n")
		return
	}
	conn, err := session.StartProcess("/bin/sh", []string{"/bin/sh", "-c", "echo up; sleep 600"})
	if err != nil {
		os.Stdout.WriteString("HOLDER-ERR " + err.Error() + "\n")
		return
	}
	line, _ := bufio.NewReader(conn).ReadString('\n')
	os.Stdout.WriteString("READY " + strings.TrimSpace(line) + "\n")
	time.Sleep(10 * time.Minute)
}

// TestReplaceExistingE2E is the development restart: a server holding a VM on
// persistent storage is killed rather than closed, so its VM keeps the name, and
// the next server has to take over both the name and the storage.vhdx.
//
// Ending the leftover session is the name half. The boot is the storage half:
// the first process a session starts is what boots its VM and attaches the VHD,
// so it is what fails if the old VM has not let go of the disk yet.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestReplaceExistingE2E -v ./providers/wslc/internal/wslcsession/
func TestReplaceExistingE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}
	storage := filepath.Join(t.TempDir(), "vm")

	holder := exec.Command(os.Args[0], "-test.run", "^TestReplaceExistingHolderE2E$", "-test.v")
	holder.Env = append(os.Environ(), "DISCOBOX_WSLC_REPLACE_HOLDER_STORAGE="+storage)
	out, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(out)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("holder exited before its VM was up: %v", err)
		}
		if strings.HasPrefix(line, "HOLDER-ERR") {
			t.Fatalf("holder: %s", line)
		}
		if strings.HasPrefix(line, "READY") {
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, lines) }()

	// What watchnbuild does on Windows: a forced termination, so the holder
	// never closes its session.
	if err := holder.Process.Kill(); err != nil {
		t.Fatalf("kill holder: %v", err)
	}
	_ = holder.Wait()
	killed := time.Now()

	// Without ReplaceExisting the name is still taken - the precondition that
	// makes this a test of anything.
	if _, err := NewSession(Options{DisplayName: replaceE2EName, StoragePath: storage, MaxStorageSizeMB: 8192}); err == nil {
		t.Skip("the killed holder's session was already gone; nothing left to replace")
	}

	session, err := NewSession(Options{DisplayName: replaceE2EName, StoragePath: storage, MaxStorageSizeMB: 8192, ReplaceExisting: true})
	if err != nil {
		t.Fatalf("NewSession(ReplaceExisting) %v after the kill: %v", time.Since(killed).Round(time.Millisecond), err)
	}
	defer func() { _ = session.Close() }()
	if !session.ReplacedExisting() {
		t.Fatal("NewSession(ReplaceExisting) reports it replaced nothing, though the name was taken")
	}
	t.Logf("took over the name %v after the kill", time.Since(killed).Round(time.Millisecond))

	answer := guestOutput(t, session, "echo booted")
	if strings.TrimSpace(answer) != "booted" {
		t.Fatalf("replacement VM answered %q", answer)
	}
	t.Logf("replacement VM booted on the same storage %v after the kill", time.Since(killed).Round(time.Millisecond))
}
