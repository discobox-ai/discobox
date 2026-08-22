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
	"testing"
	"time"

	"github.com/discobox-ai/discobox/execstream/frame"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
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
// nothing. This pins the stop and the resume.
func TestRunShimSuspendAndResume(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("process state is read from /proc")
	}
	dir := shimDir(t)
	logs := newFakeLogSink()
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
