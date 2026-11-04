package execs

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/programstatus"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
	"github.com/discobox-ai/discobox/sandbox-agent/shimruntime"
)

// A terminal is typed into, waited on, and read through its shim with nothing
// attached (ADR 0137).
func TestShimTerminalIsTypedWaitedOnAndRead(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_terminal_io",
			Command:     []string{"sh", "-c", "stty -echo; while read line; do echo \"got $line\"; done"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
			Rows:        10,
			Cols:        40,
			TTY:         true,
		})
	}()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	_ = conn.Close()
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	input := ShimInput{Input: []shimruntime.InputPart{{Text: "hello"}, {Key: "Enter"}}}
	if _, err := shimproxy.CallJSON[struct{}](ctx, socketPath, http.MethodPost, "/input", input); err != nil {
		t.Fatalf("input: %v", err)
	}
	wait, err := shimproxy.CallJSON[ShimWaitResult](ctx, socketPath, http.MethodPost, "/wait", ShimWait{QuietSeconds: 1, TimeoutSeconds: 10})
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if wait.Reason != shimruntime.WaitQuiet || wait.OutputAt == nil {
		t.Fatalf("wait = %+v, want quiet with an output time", wait)
	}
	screen, err := shimproxy.CallJSON[shimruntime.ScreenText](ctx, socketPath, http.MethodGet, "/screen", nil)
	if err != nil {
		t.Fatalf("screen: %v", err)
	}
	if screen.Exited {
		t.Fatal("screen reports exited while the process runs")
	}
	if !slices.ContainsFunc(screen.Lines, func(line string) bool { return strings.TrimSpace(line) == "got hello" }) {
		t.Fatalf("screen lines = %q, want a line reading got hello", screen.Lines)
	}

	_, err = shimproxy.CallJSON[struct{}](ctx, socketPath, http.MethodPost, "/input", ShimInput{Input: []shimruntime.InputPart{{Key: "Hyper"}}})
	var shimErr *shimproxy.ShimError
	if !errors.As(err, &shimErr) || shimErr.Status != http.StatusBadRequest {
		t.Fatalf("unknown key err = %v, want a 400", err)
	}

	if _, err := shimproxy.CallJSON[struct{}](ctx, socketPath, http.MethodPost, "/input", ShimInput{Input: []shimruntime.InputPart{{Key: "C-d"}}}); err != nil {
		t.Fatalf("input C-d: %v", err)
	}
	wait, err = shimproxy.CallJSON[ShimWaitResult](ctx, socketPath, http.MethodPost, "/wait", ShimWait{Exit: true, TimeoutSeconds: 10})
	if err != nil {
		t.Fatalf("wait exit: %v", err)
	}
	if wait.Reason != shimruntime.WaitExit {
		t.Fatalf("wait = %+v, want exit", wait)
	}
	// The last screen stays readable through the linger, and says it is final.
	screen, err = shimproxy.CallJSON[shimruntime.ScreenText](ctx, socketPath, http.MethodGet, "/screen", nil)
	if err != nil {
		t.Fatalf("screen after exit: %v", err)
	}
	if !screen.Exited {
		t.Fatalf("screen after exit = %+v, want exited", screen)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A wait counting from the terminal's last input ends on the program's report
// after it, never on the state that held before it (ADR 26-11-04-204 §§4-5).
func TestShimWaitEndsOnAProgramReportAfterTheInput(t *testing.T) {
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	idle, working, done := `printf '\033]7501;state=idle\033\\'`, `printf '\033]7501;state=working\033\\'`, `printf '\033]7501;state=done\033\\'`
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_program_wait",
			Command:     []string{"sh", "-c", "stty -echo; " + idle + "; while read line; do " + working + "; sleep 0.2; " + done + "; done"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
			Rows:        10,
			Cols:        40,
			TTY:         true,
		})
	}()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	_ = conn.Close()
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}
	finished := []shimruntime.ProgramMatch{{State: programstatus.Idle}, {State: programstatus.Done}, {State: programstatus.Error}}

	// Idle before anything was sent: a wait counting from now does not end on it.
	time.Sleep(200 * time.Millisecond)
	now := time.Now().UTC()
	wait, err := shimproxy.CallJSON[ShimWaitResult](ctx, socketPath, http.MethodPost, "/wait", ShimWait{Since: &now, Program: finished, TimeoutSeconds: 1})
	if err != nil || wait.Reason != shimruntime.WaitTimeout {
		t.Fatalf("wait before input = %+v, %v; want timeout", wait, err)
	}

	if _, err := shimproxy.CallJSON[struct{}](ctx, socketPath, http.MethodPost, "/input", ShimInput{Input: []shimruntime.InputPart{{Text: "go"}, {Key: "Enter"}}}); err != nil {
		t.Fatalf("input: %v", err)
	}
	screen, err := shimproxy.CallJSON[shimruntime.ScreenText](ctx, socketPath, http.MethodGet, "/screen", nil)
	if err != nil || screen.SentAt == nil {
		t.Fatalf("screen after input = %+v, %v; want a sent time", screen.SentAt, err)
	}
	// Made late, after the turn may already have ended: counting from the
	// input, it still finds the report.
	time.Sleep(400 * time.Millisecond)
	wait, err = shimproxy.CallJSON[ShimWaitResult](ctx, socketPath, http.MethodPost, "/wait", ShimWait{Since: screen.SentAt, Program: finished, TimeoutSeconds: 5})
	if err != nil {
		t.Fatalf("wait after input: %v", err)
	}
	if wait.Reason != shimruntime.WaitProgram || wait.Program == nil || wait.Program.State != programstatus.Done || !wait.Program.ReportedAt.After(*screen.SentAt) {
		t.Fatalf("wait after input = %+v (%+v), want the done reported after the input", wait, wait.Program)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}

// A wait may hold longer than this server bounds an ordinary response at, so
// it sets its own deadline; without one the caller reads an unexpected EOF
// instead of the answer (ADR 0137 §3).
func TestShimWaitOutlivesTheServerWriteTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out the shim's write timeout")
	}
	dir := shimDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	socketPath := filepath.Join(dir, "shim.sock")
	errCh := make(chan error, 1)
	go func() {
		errCh <- RunShim(ctx, ShimConfig{
			ExecID:      "exec_long_wait",
			Command:     []string{"sh", "-c", "stty -echo; sleep 600"},
			Workdir:     dir,
			SocketPath:  socketPath,
			RuntimePath: filepath.Join(dir, "runtime.json"),
			Logs:        newFakeLogSink(),
			Rows:        10,
			Cols:        40,
			TTY:         true,
		})
	}()
	conn, err := shimproxy.Dial(ctx, socketPath, 5*time.Second)
	if err != nil {
		t.Fatalf("dial shim: %v", err)
	}
	_ = conn.Close()
	if _, err := shimproxy.StartJSON[Exec](ctx, socketPath); err != nil {
		t.Fatalf("start shim: %v", err)
	}

	quiet := int(shimWriteTimeout/time.Second) + 5
	began := time.Now()
	wait, err := shimproxy.CallJSON[ShimWaitResult](ctx, socketPath, http.MethodPost, "/wait", ShimWait{QuietSeconds: quiet, TimeoutSeconds: maxWaitSeconds})
	if err != nil {
		t.Fatalf("wait longer than the write timeout: %v", err)
	}
	if wait.Reason != shimruntime.WaitQuiet {
		t.Fatalf("wait = %+v, want quiet", wait)
	}
	if elapsed := time.Since(began); elapsed < shimWriteTimeout {
		t.Fatalf("answered after %v, which did not outlast the %v write timeout", elapsed, shimWriteTimeout)
	}

	cancel()
	if err := <-errCh; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("run shim: %v", err)
	}
}
