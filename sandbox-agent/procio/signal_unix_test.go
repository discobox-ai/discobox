//go:build !windows

package procio

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A signal to a process on a terminal goes where the terminal would send it:
// the foreground job. An interactive shell — a primary terminal's `bash -l` —
// gives each command it runs a process group of its own and hands it the
// terminal, so the shell's own group is the shell alone, which ignores
// SIGTERM; signaling that group never reached the harness on the screen.
func TestSignalOnATerminalReachesTheForegroundJob(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is the interactive shell with job control this case needs")
	}
	dir := t.TempDir()
	ready, got := dir+"/ready", dir+"/got"
	p := start(t, Options{
		Command: []string{bash, "--norc", "--noprofile", "-i"},
		Env:     append(os.Environ(), "PS1=$ "),
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()

	job := fmt.Sprintf(`sh -c 'trap ": > %s; exit 0" TERM; : > %s; while :; do sleep 0.1; done'`+"\n", got, ready)
	if _, err := p.WriteInput([]byte(job)); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, ready, "the job to start")

	delivered, err := p.Signal("TERM")
	if err != nil {
		t.Fatalf("signal: %v", err)
	}
	if delivered.Delivered != "SIGTERM" {
		t.Fatalf("delivered %+v, want SIGTERM", delivered)
	}
	waitForFile(t, got, "the foreground job to get SIGTERM")

	// The shell was not the target and is still there, as a local terminal
	// leaves it.
	exited := make(chan struct{})
	go func() { p.Wait(); close(exited) }()
	select {
	case <-exited:
		t.Fatal("the shell exited; the signal was the foreground job's")
	case <-time.After(300 * time.Millisecond):
	}
}

// Suspend and resume stay on the process's own group, not the job in front: a
// stopped job hands the terminal back to its shell, and a resume from outside
// would leave it in the background, to stop again on its next read.
func TestSuspendOnATerminalStaysOffTheForegroundJob(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process state is read from /proc")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is the interactive shell with job control this case needs")
	}
	dir := t.TempDir()
	pidFile := dir + "/pid"
	p := start(t, Options{
		Command: []string{bash, "--norc", "--noprofile", "-i"},
		Env:     append(os.Environ(), "PS1=$ "),
		TTY:     true,
		Winsize: &pty.Winsize{Rows: 24, Cols: 80},
	})
	go func() { _, _ = io.Copy(io.Discard, p.Stdout()) }()

	// exec makes the job's group leader the sleep itself, whose state is then
	// the job's.
	job := fmt.Sprintf(`sh -c 'echo $$ > %s.tmp && mv %s.tmp %s && exec sleep 600'`+"\n", pidFile, pidFile, pidFile)
	if _, err := p.WriteInput([]byte(job)); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, pidFile, "the job to start")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	jobPID, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	shell := int(p.PID())

	if _, err := p.Signal("TSTP"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	waitForPIDState(t, shell, "T")
	waitForPIDState(t, jobPID, "S")
	if _, err := p.Signal("CONT"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitForPIDState(t, shell, "S")
	waitForPIDState(t, jobPID, "S")
}

func waitForFile(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
