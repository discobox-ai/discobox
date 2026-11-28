package execs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/execstream/frame"
	"github.com/discobox-ai/discobox/sandbox-agent/procio"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
	"github.com/discobox-ai/discobox/sandbox-agent/shimruntime"
	"github.com/discobox-ai/x/shorttmp"
)

// shimDir is a scratch directory short enough to hold the shim's socket, and
// the one place every shim test states its shared premise: the child is a POSIX
// command — sh, sleep, bash — and the terminal cases put it on a PTY, which
// procio deliberately does not open on Windows. A shim only ever runs inside
// the Linux sandbox, so neither half is a Windows host's to provide.
func shimDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("a shim runs a POSIX child, on a PTY for the terminal cases; the sandbox is Linux")
	}
	return shorttmp.Dir(t)
}

func TestRunShimSendsOutputBeforeExitFrame(t *testing.T) {
	dir := shimDir(t)
	logs := newFakeLogSink()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sh", "-c", "printf hi; exit 7"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
		})
	}()

	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}

	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	var output []byte
	var exit frame.ExitPayload
	for {
		next, err := frame.Read(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				t.Fatal("attach closed before exit frame")
			}
			t.Fatalf("read frame: %v", err)
		}
		switch next.Type {
		case frame.Stdout:
			output = append(output, next.Payload...)
		case frame.Exit:
			var decodeErr error
			exit, decodeErr = frame.DecodeExit(next.Payload)
			if decodeErr != nil {
				t.Fatalf("decode exit: %v", decodeErr)
			}
			goto done
		default:
			t.Fatalf("unexpected frame type %d", next.Type)
		}
	}

done:
	if string(output) != "hi" {
		t.Fatalf("output = %q, want hi", string(output))
	}
	if exit.ExitCode == nil || *exit.ExitCode != 7 {
		if exit.ExitCode == nil {
			t.Fatalf("exit code was not sent")
		}
		t.Fatalf("exit code = %d, want 7", *exit.ExitCode)
	}
	// Logs are flushed by the time the exit frame arrives, so read them before
	// tearing down.
	entries, err := ReadExecLog(ctx, logs, "exec_test")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	var logged []byte
	for _, entry := range entries {
		if entry.Stream == LogStreamStdout {
			logged = append(logged, entry.Data...)
		}
	}
	if string(logged) != "hi" {
		t.Fatalf("logged output = %q, want hi", string(logged))
	}
	// The shim lingers after exit so a late attacher can still replay + read the
	// exit code; cancel to end the linger and shut it down.
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

func TestRunShimUsesResizeFrameBeforeStart(t *testing.T) {
	dir := shimDir(t)
	logs := newFakeLogSink()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sizePath := filepath.Join(dir, "size.txt")
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sh", "-c", "sleep 0.2; stty size > " + sizePath},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
			Rows:        24,
			Cols:        80,
			TTY:         true,
		})
	}()

	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}

	payload, err := frame.EncodeResize(101, 33)
	if err != nil {
		t.Fatalf("encode resize: %v", err)
	}
	if err := frame.Write(conn, frame.Resize, payload); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	// The child writes the size file and exits; wait for that (the shim lingers
	// after exit), then cancel to end the linger.
	// Wait for content, not just for the file: the shell's redirect creates it
	// before stty writes, so an existence check reads an empty file under load.
	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		var readErr error
		if data, readErr = os.ReadFile(sizePath); readErr == nil && len(data) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("size file not written: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
	if got := string(data); got != "33 101\n" {
		t.Fatalf("size = %q, want %q", got, "33 101\n")
	}
}

// attachReadExit opens an attach to the shim, optionally starts the process, and
// reads frames until the exit frame, returning it.
func attachReadExit(ctx context.Context, t *testing.T, socketPath string, start bool) frame.ExitPayload {
	t.Helper()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	_ = resp.Body.Close()
	if start {
		if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
			t.Fatalf("start shim: %v", err)
		}
	}
	for {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if next.Type == frame.Exit {
			exit, err := frame.DecodeExit(next.Payload)
			if err != nil {
				t.Fatalf("decode exit: %v", err)
			}
			return exit
		}
	}
}

// A client that attaches after the process has already exited still receives the
// exit frame (and code) rather than a bare disconnect, because the shim lingers.
func TestRunShimLingersForLateAttach(t *testing.T) {
	dir := shimDir(t)
	logs := newFakeLogSink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_late",
			Command:     []string{"sh", "-c", "printf bye; exit 3"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
		})
	}()

	// A current attacher observes the live exit (proving the process has ended).
	if exit := attachReadExit(ctx, t, socketPath, true); exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("first attach exit = %v, want 3", exit.ExitCode)
	}
	// A second attacher connects after the exit; the lingering shim still delivers
	// the exit frame + code.
	if exit := attachReadExit(ctx, t, socketPath, false); exit.ExitCode == nil || *exit.ExitCode != 3 {
		t.Fatalf("late attach exit = %v, want 3", exit.ExitCode)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A pipe exec keeps its streams apart: stdout arrives as frame.Stdout and
// stderr as frame.Stderr, so a client can route each the way a local command
// does. A TTY exec has no such split — the PTY merges them before the shim.
func TestRunShimSeparatesStderrFrames(t *testing.T) {
	dir := shimDir(t)
	logs := newFakeLogSink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_streams",
			Command:     []string{"sh", "-c", "printf out; printf err >&2"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
		})
	}()

	reader := attachShimForTest(ctx, t, socketPath)
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	var stdout, stderr []byte
	for done := false; !done; {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch next.Type {
		case frame.Stdout:
			stdout = append(stdout, next.Payload...)
		case frame.Stderr:
			stderr = append(stderr, next.Payload...)
		case frame.Exit:
			done = true
		default:
			t.Fatalf("unexpected frame type %d", next.Type)
		}
	}
	if string(stdout) != "out" {
		t.Fatalf("stdout frames = %q, want out", string(stdout))
	}
	if string(stderr) != "err" {
		t.Fatalf("stderr frames = %q, want err", string(stderr))
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// attachShimForTest opens an attach stream to the shim and returns its frame
// reader.
func attachShimForTest(ctx context.Context, t *testing.T, socketPath string) *bufio.Reader {
	t.Helper()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}
	return reader
}

// A TTY exec writes everything to one PTY, so both of its streams arrive as
// frame.Stdout and frame.Stderr is simply never used. Nothing on the wire — or
// in the log — lets a client tell this apart from a pipe exec that wrote
// nothing to stderr, which is the point: clients do not special-case TTYs.
func TestRunShimTTYReportsEverythingAsStdout(t *testing.T) {
	dir := shimDir(t)
	logs := newFakeLogSink()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_tty_streams",
			Command:     []string{"sh", "-c", "printf out; printf err >&2"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
			Rows:        24,
			Cols:        80,
			TTY:         true,
		})
	}()

	reader := attachShimForTest(ctx, t, socketPath)
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	var stdout []byte
	for done := false; !done; {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch next.Type {
		case frame.Stdout:
			stdout = append(stdout, next.Payload...)
		case frame.Stderr:
			t.Fatal("a TTY exec must never send a stderr frame")
		case frame.Exit:
			done = true
		default:
			t.Fatalf("unexpected frame type %d", next.Type)
		}
	}
	// The PTY interleaves both writes; order is the process's business, so assert
	// only that everything arrived on stdout.
	if !bytes.Contains(stdout, []byte("out")) || !bytes.Contains(stdout, []byte("err")) {
		t.Fatalf("stdout frames = %q, want both writes merged onto stdout", string(stdout))
	}

	entries, err := ReadExecLog(ctx, logs, "exec_tty_streams")
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	for _, entry := range entries {
		if entry.Stream == LogStreamStderr {
			t.Fatalf("a TTY exec must not log a stderr stream, got %q", entry.Data)
		}
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A client that attaches and then starts the process must receive everything it
// wrote, including output produced in the instant between the 101 response and
// the shim joining the attacher to the broadcast set. A fast command's entire
// output lives in that window.
func TestRunShimDoesNotLoseOutputRacingAttach(t *testing.T) {
	for i := range 25 {
		dir := shimDir(t)
		logs := newFakeLogSink()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		socketPath := filepath.Join(dir, "shim.sock")
		errCh := make(chan error, 1)
		go func() {
			errCh <- RunShim(ctx, ShimConfig{
				ExecID:      "exec_race",
				Command:     []string{"printf", "hi"},
				Workdir:     dir,
				SocketPath:  socketPath,
				RuntimePath: filepath.Join(dir, "runtime.json"),
				Logs:        logs,
			})
		}()

		reader := attachShimForTest(ctx, t, socketPath)
		if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
			t.Fatalf("start shim: %v", err)
		}
		var stdout []byte
		for done := false; !done; {
			next, err := frame.Read(reader)
			if err != nil {
				t.Fatalf("read frame: %v", err)
			}
			switch next.Type {
			case frame.Stdout:
				stdout = append(stdout, next.Payload...)
			case frame.Exit:
				done = true
			}
		}
		if string(stdout) != "hi" {
			t.Fatalf("run %d: stdout = %q, want hi (output lost in the attach window)", i, string(stdout))
		}
		cancel()
		if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("run shim: %v", err)
		}
	}
}

// A suspend request must actually stop the process. Every exec runs with Setsid,
// which by definition orphans its process group, and the kernel discards
// SIGTSTP sent to an orphaned group — so the obvious mapping silently does
// nothing. This pins the stop and the resume, and that the mapping is said in
// the exec's audit trail while a signal delivered as itself is not.
func TestRunShimSuspendAndResume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process state is read from /proc")
	}
	dir := shimDir(t)
	logs := newFakeLogSink()
	events := &recordedEvents{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_suspend",
			Command:     []string{"sleep", "30"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        logs,
			Events:      events,
		})
	}()

	conn, reader := attachShimConnForTest(ctx, t, socketPath)
	started, err := shimproxy.StartJSON[Exec](ctx, socketPath)
	if err != nil {
		t.Fatalf("start shim: %v", err)
	}
	pid := int(started.PID)
	waitForProcessState(t, pid, "S", "process never started running")

	if err := frame.Write(conn, frame.Signal, []byte("TSTP")); err != nil {
		t.Fatalf("write suspend frame: %v", err)
	}
	waitForProcessState(t, pid, "T", "suspend did not stop the process")

	if err := frame.Write(conn, frame.Signal, []byte("CONT")); err != nil {
		t.Fatalf("write resume frame: %v", err)
	}
	waitForProcessState(t, pid, "S", "resume did not restart the process")

	// The frames are applied in order, so by the time the resume has landed
	// the suspend's event has been recorded, and the resume recorded none.
	recorded := events.snapshot()
	if len(recorded) != 1 {
		t.Fatalf("events = %+v, want the suspend's mapping alone", recorded)
	}
	got := recorded[0]
	if got.execID != "exec_suspend" || got.typ != "exec.signal.mapped" ||
		got.details["signal"] != "TSTP" || got.details["delivered"] != "SIGSTOP" ||
		!strings.Contains(got.message, "signal TSTP delivered as SIGSTOP: sleep 30") ||
		!strings.Contains(fmt.Sprint(got.details["reason"]), "orphaned") {
		t.Fatalf("event = %+v, want TSTP recorded as delivered by SIGSTOP, and why", got)
	}

	_ = reader
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A harness terminal never execs its command directly — it execs a shell and
// types the command in, so the command runs as the shell's foreground job
// rather than as the exec's own (orphaned) session leader. This is what makes
// Ctrl-Z work at all: TestRunShimSuspendAndResume above pins that an orphaned
// session leader's process group discards SIGTSTP outright (a Signal frame
// has to map it to SIGSTOP to get anywhere). Here the same raw Ctrl-Z byte a
// real terminal sends — a frame.Input, not a frame.Signal — must stop the
// child while leaving the shell itself alive to hand back a prompt.
func TestRunShimStartupCommandGetsRealJobControl(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process state is read from /proc")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:         "exec_startup",
			Command:        []string{"bash", "--norc", "--noprofile", "-i"},
			StartupCommand: []string{"sleep", "30"},
			Workdir:        dir,
			SocketPath:     socketPath,
			RuntimePath:    filepath.Join(dir, "runtime.json"),
			Logs:           newFakeLogSink(),
			Rows:           24,
			Cols:           80,
			TTY:            true,
		})
	}()

	conn, _ := attachShimConnForTest(ctx, t, socketPath)
	started, err := shimproxy.StartJSON[Exec](ctx, socketPath)
	if err != nil {
		t.Fatalf("start shim: %v", err)
	}
	shellPID := int(started.PID)

	var childPID int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if pid, ok := findChildPID(shellPID, "sleep"); ok {
			childPID = pid
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatalf("typed-in startup command never appeared as a child of the shell (pid %d)", shellPID)
	}
	waitForProcessState(t, childPID, "S", "startup command never started running")

	if err := frame.Write(conn, frame.Input, []byte{0x1a}); err != nil {
		t.Fatalf("write ctrl-z byte: %v", err)
	}
	waitForProcessState(t, childPID, "T", "ctrl-z did not stop the shell's child job")
	waitForProcessState(t, shellPID, "S", "the shell must still be running after its child stopped, ready to hand back a prompt")

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// TestRunShimReportsAttacherCount confirms /status reports the live attacher
// count (not a stale snapshot), since sandbox-agent's status endpoint relies
// on this to report active terminal/exec connections.
func TestRunShimReportsAttacherCount(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sh", "-c", "sleep 0.3"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
		})
	}()

	status, err := waitForStatus(ctx, socketPath)
	if err != nil {
		t.Fatalf("status before attach: %v", err)
	}
	if status.AttacherCount != 0 {
		t.Fatalf("AttacherCount before attach = %d, want 0", status.AttacherCount)
	}

	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("attach status = %s", resp.Status)
	}
	// A lower bound the leave has to beat. The shim recorded the connect before
	// it answered it, and it cannot record the leave before the program started
	// below has run and exited. A bound taken after the exit frame would race the
	// shim, which ends the attach itself once the program exits.
	attachedAt := time.Now().UTC()
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	if err := waitForAttacherCount(ctx, socketPath, 1); err != nil {
		t.Fatalf("attacher count did not reach 1 after attach: %v", err)
	}

	// Read until the exit frame rather than force-killing the command via
	// cancel: the exit frame is only sent after the shim's wait() has fully
	// drained output and closed the log, so waiting for it (as the other
	// RunShim tests do) avoids racing that async cleanup against this test's
	// TempDir removal.
	for {
		next, err := frame.Read(reader)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if next.Type == frame.Exit {
			break
		}
	}

	conn.Close()

	if err := waitForAttacherCount(ctx, socketPath, 0); err != nil {
		t.Fatalf("attacher count did not return to 0 after detach: %v", err)
	}
	// Leaving is access: the idle stop's clock starts when the last client
	// left, not at its last keystroke (ADR 0108 §2).
	deadline := time.Now().Add(5 * time.Second)
	var last *time.Time
	for {
		if status, err := shimproxy.StatusJSON[Exec](ctx, socketPath); err == nil {
			last = status.LastAccessedAt
			if last != nil && last.After(attachedAt) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("last accessed at = %v after detach, want later than the attach at %v", last, attachedAt)
		}
		time.Sleep(25 * time.Millisecond)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// findChildPID scans /proc for a running process whose parent pid is parent
// and whose comm matches name.
func findChildPID(parent int, name string) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		open := bytes.IndexByte(data, '(')
		closeParen := bytes.LastIndexByte(data, ')')
		if open < 0 || closeParen < 0 || closeParen <= open || closeParen+2 >= len(data) {
			continue
		}
		if string(data[open+1:closeParen]) != name {
			continue
		}
		fields := bytes.Fields(data[closeParen+2:])
		if len(fields) < 2 {
			continue
		}
		ppid, err := strconv.Atoi(string(fields[1]))
		if err == nil && ppid == parent {
			return pid, true
		}
	}
	return 0, false
}

func waitForStatus(ctx context.Context, socketPath string) (Exec, error) {
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		status, err := shimproxy.StatusJSON[Exec](ctx, socketPath)
		if err == nil {
			return status, nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}
	return Exec{}, lastErr
}

func waitForAttacherCount(ctx context.Context, socketPath string, want int) error {
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		status, err := shimproxy.StatusJSON[Exec](ctx, socketPath)
		if err == nil {
			got = status.AttacherCount
			if got == want {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("attacher count = %d, want %d", got, want)
}

// waitForProcessState polls /proc until the process reaches want ("S" running
// or sleeping, "T" stopped).
func waitForProcessState(t *testing.T, pid int, want, message string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err == nil {
			// The state field follows the parenthesized comm, which may itself
			// contain spaces or parentheses.
			if idx := bytes.LastIndexByte(data, ')'); idx >= 0 && idx+2 < len(data) {
				got = string(data[idx+2 : idx+3])
				if got == want {
					return
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s: state = %q, want %q", message, got, want)
}

// attachShimConnForTest opens an attach stream and returns both ends so the
// caller can write client frames as well as read.
func attachShimConnForTest(ctx context.Context, t *testing.T, socketPath string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/attach", nil)
	if err != nil {
		t.Fatalf("new attach request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "discobox-sandbox-exec")
	if err := req.Write(conn); err != nil {
		t.Fatalf("write attach request: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	_ = resp.Body.Close()
	return conn, reader
}

// recordedEvents is an ExecEventRecorder that keeps what it was given.
type recordedEvents struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	execID, typ, message string
	details              map[string]any
}

func (r *recordedEvents) RecordExecEvent(_ context.Context, execID, typ, message string, details map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recordedEvent{execID: execID, typ: typ, message: message, details: details})
	return nil
}

func (r *recordedEvents) snapshot() []recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedEvent(nil), r.events...)
}

// A Delivery names the mechanism chosen before the platform call ran, so a
// call that failed must not be recorded as delivered: it is exec.signal.failed,
// with the error.
func TestRecordSignalNeverCallsAFailedDeliveryDelivered(t *testing.T) {
	events := &recordedEvents{}
	r := &shimRuntime{cfg: ShimConfig{ExecID: "exec_failed", Events: events}, status: Exec{Command: []string{"sleep", "30"}}}
	suspend := procio.Delivery{Requested: "TSTP", Delivered: "NtSuspendProcess", Reason: "a Windows process has no SIGSTOP"}

	r.recordSignal(suspend, errors.New("NtSuspendProcess: NTSTATUS 0xc0000008"))
	r.recordSignal(suspend, nil)

	got := events.snapshot()
	if len(got) != 2 {
		t.Fatalf("events = %+v, want one failure and one delivery", got)
	}
	failed, delivered := got[0], got[1]
	if failed.typ != "exec.signal.failed" || strings.Contains(failed.message, "delivered as") ||
		!strings.Contains(failed.message, "failed as NtSuspendProcess (NtSuspendProcess: NTSTATUS 0xc0000008)") ||
		failed.details["error"] != "NtSuspendProcess: NTSTATUS 0xc0000008" {
		t.Fatalf("failed = %+v, want it recorded as a failure with its error", failed)
	}
	if delivered.typ != "exec.signal.mapped" || !strings.Contains(delivered.message, "signal TSTP delivered as NtSuspendProcess: sleep 30") {
		t.Fatalf("delivered = %+v, want the successful mapping", delivered)
	}
}

// The shim's own environment is not the exec's (#1076), so a command named
// bare is found on the PATH the exec gives it, as it was when the exec's
// environment was the shim's too.
func TestRunShimFindsTheCommandOnTheExecsPath(t *testing.T) {
	dir := shimDir(t)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "only-on-exec-path"), []byte("#!/bin/sh\nexit 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(os.Getenv("PATH"), bin) {
		t.Fatal("the test's own PATH already holds the command")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_path",
			Command:     []string{"only-on-exec-path"},
			Workdir:     dir,
			Env:         map[string]string{"PATH": bin + string(os.PathListSeparator) + os.Getenv("PATH")},
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
		})
	}()
	exit := attachReadExit(ctx, t, socketPath, true)
	if exit.ExitCode == nil || *exit.ExitCode != 5 {
		t.Fatalf("exit = %+v, want the command on the exec's PATH to run and exit 5", exit)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A shim's process is set by the start request while the attach stream and the
// shutdown path read it from their own goroutines: a client already attached
// may send a frame, and RunShim may be told to stop, before the start has
// returned. Run under -race, this fails on any unsynchronized access to proc.
func TestShimProcIsSynchronizedWithStart(t *testing.T) {
	dir := shimDir(t)
	logger, err := NewAsyncLogger(newFakeLogSink(), "exec_test", nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	r := &shimRuntime{
		cfg: ShimConfig{
			ExecID:      "exec_test",
			Command:     []string{"sleep", "30"},
			Workdir:     dir,
			RuntimePath: filepath.Join(dir, "runtime.json"),
		},
		logger: logger,
		done:   make(chan struct{}),
		exited: make(chan struct{}),
	}
	r.stream = shimruntime.New("discobox-sandbox-exec", r.done, r.handleAttachFrame)
	defer r.close()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		if err := r.startCommand(); err != nil {
			t.Errorf("start: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		_ = r.handleAttachFrame(frame.Frame{Type: frame.Signal, Payload: []byte("SIGCONT")})
	}()
	go func() {
		defer wg.Done()
		r.terminate()
	}()
	wg.Wait()
}

func TestRunShimReportsAMissingCommandAsExit127(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_missing",
			Command:     []string{"nonexistent-cmd-xyz"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
		})
	}()
	// A command that is not there is the user's error, as a shell reports it:
	// the exec starts, says so on stderr, and exits 127. It is not the shim
	// failing to start.
	exit := attachReadExit(ctx, t, socketPath, true)
	if exit.ExitCode == nil || *exit.ExitCode != 127 {
		t.Fatalf("exit = %+v, want 127 for a command that is not on the PATH", exit)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A command the shim cannot run ends the exec with a shell's exit status for
// it: the reason on the exec's stderr (its one stream, on a terminal), in the
// log as well as live, and a non-zero exit. No process ran, so the record
// carries no start time or PID, and a second start is a no-op. (The first case
// is TestRunShimReportsAMissingCommandAsExit127, the issue's reproduction,
// with its output checked too.)
func TestRunShimEndsACommandItCannotRunWithTheShellsStatus(t *testing.T) {
	dir := shimDir(t)
	notExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badFormat := filepath.Join(dir, "bad-format")
	if err := os.WriteFile(badFormat, []byte("\x00\x01not a program"), 0o755); err != nil {
		t.Fatal(err)
	}
	badInterpreter := filepath.Join(dir, "bad-interpreter")
	if err := os.WriteFile(badInterpreter, []byte("#!/nonexistent/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(dir, "missing")
	cases := []struct {
		name    string
		command string
		tty     bool
		code    int64
		output  string
	}{
		{"not on the PATH", "nonexistent-cmd-xyz", false, 127, "nonexistent-cmd-xyz: command not found\n"},
		{"not on the PATH, on a terminal", "nonexistent-cmd-xyz", true, 127, "nonexistent-cmd-xyz: command not found\r\n"},
		{"a path that is not there", missingPath, false, 127, missingPath + ": no such file or directory\n"},
		{"not executable", notExecutable, false, 126, notExecutable + ": permission denied\n"},
		{"not a program", badFormat, false, 126, badFormat + ": cannot execute: exec format error\n"},
		{"a directory", dir, false, 126, dir + ": permission denied\n"},
		{"an interpreter that is not there", badInterpreter, false, 126, badInterpreter + ": cannot execute: required file not found\n"},
		{"relative to the working directory", "./missing", false, 127, "./missing: no such file or directory\n"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			logs := newFakeLogSink()
			execID := fmt.Sprintf("exec_not_runnable_%d", i)
			socketPath := filepath.Join(dir, fmt.Sprintf("shim%d.sock", i))
			runtimePath := filepath.Join(dir, fmt.Sprintf("runtime%d.json", i))
			errCh := make(chan error, 1)
			go func() {
				errCh <- RunShim(ctx, ShimConfig{
					ExecID:      execID,
					Command:     []string{tc.command, "arg"},
					Workdir:     dir,
					SocketPath:  socketPath,
					RuntimePath: runtimePath,
					Logs:        logs,
					Rows:        24,
					Cols:        80,
					TTY:         tc.tty,
				})
			}()
			reader := attachShimForTest(ctx, t, socketPath)
			started, err := shimproxy.StartJSON[Exec](ctx, socketPath)
			if err != nil {
				t.Fatalf("start shim: %v", err)
			}
			if started.Status != StatusFailed || started.ExitCode == nil || *started.ExitCode != tc.code {
				t.Fatalf("start = %+v, want exit %d", started, tc.code)
			}

			want, other := frame.Stderr, frame.Stdout
			if tc.tty {
				want, other = frame.Stdout, frame.Stderr
			}
			var output []byte
			for done := false; !done; {
				next, err := frame.Read(reader)
				if err != nil {
					t.Fatalf("read frame: %v", err)
				}
				switch next.Type {
				case want:
					output = append(output, next.Payload...)
				case other:
					t.Fatalf("unexpected frame type %d: %q", next.Type, next.Payload)
				case frame.Exit:
					exit, err := frame.DecodeExit(next.Payload)
					if err != nil {
						t.Fatalf("decode exit: %v", err)
					}
					// Failed with the code, as a command that ran and exited
					// non-zero is, and why as its error.
					if exit.Status != string(StatusFailed) || exit.ExitCode == nil || *exit.ExitCode != tc.code || exit.Error != strings.TrimSpace(tc.output) {
						t.Fatalf("exit = %+v, want exit %d with why", exit, tc.code)
					}
					done = true
				}
			}
			if string(output) != tc.output {
				t.Fatalf("output = %q, want %q", output, tc.output)
			}

			entries, err := ReadExecLog(ctx, logs, execID)
			if err != nil {
				t.Fatalf("read logs: %v", err)
			}
			var logged []byte
			for _, entry := range entries {
				logged = append(logged, entry.Data...)
			}
			if string(logged) != tc.output {
				t.Fatalf("log = %q, want %q", logged, tc.output)
			}

			record, err := readRuntime(runtimePath)
			if err != nil {
				t.Fatalf("read runtime: %v", err)
			}
			if record.Status != StatusFailed || record.ExitCode == nil || *record.ExitCode != tc.code ||
				record.ExitedAt == nil || record.StartedAt != nil || record.PID != 0 {
				t.Fatalf("runtime = %+v, want exit %d, never started", record, tc.code)
			}
			again, err := shimproxy.StartJSON[Exec](ctx, socketPath)
			if err != nil || again.Status != StatusFailed || again.ExitCode == nil || *again.ExitCode != tc.code {
				t.Fatalf("second start = %+v, %v; want the exit it already had", again, err)
			}

			cancel()
			if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("run shim: %v", err)
			}
		})
	}
}

// A working directory the command cannot be run in is the exec failing to
// start, not the command: it stays an error, never a 127 that would blame the
// command's name.
func TestRunShimFailsToStartInAMissingWorkdir(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_no_workdir",
			Command:     []string{"true"},
			Workdir:     filepath.Join(dir, "missing"),
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
		})
	}()
	attachShimForTest(ctx, t, socketPath)
	if got, err := shimproxy.StartJSON[Exec](ctx, socketPath); err == nil {
		t.Fatalf("start = %+v, want a failed start", got)
	}
	record, err := readRuntime(filepath.Join(dir, "runtime.json"))
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	if record.Status != StatusFailed || record.ExitCode != nil {
		t.Fatalf("runtime = %+v, want a failed start", record)
	}
	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}
