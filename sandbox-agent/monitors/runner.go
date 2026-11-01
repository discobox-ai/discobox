package monitors

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// brokenRecheck is how often an instance whose declaration cannot run
	// looks at it again.
	brokenRecheck = 30 * time.Second
	// maxBackoff caps how long a failing streaming monitor waits to restart,
	// and stableRun is how long a run must last for its failure to start the
	// backoff over.
	maxBackoff = 5 * time.Minute
	stableRun  = time.Minute
	// stderrTail is how much of a failed run's stderr its monitor.failed
	// event carries.
	stderrTail = 4 << 10
)

// Runner runs one instance: its monitor's script, once per interval or kept
// running, with every line it prints queued as an event (ADR 26-10-31-073 §§3-4).
//
// It is what the instance's exec runs, as the sandbox user. It writes the
// instance's status and stderr log, and queues events of its own when the
// monitor starts or stops failing. When the instance ends itself — a final
// event, or a streaming monitor exiting successfully — it removes the
// instance's file, which is what tells the agent not to start it again.
type Runner struct {
	Dirs     Dirs
	Instance Instance
	// Emit takes each event. Nil queues it; `test` prints it instead.
	Emit func(Event) error
	// Test runs the instance for `discobox-monitor test`: nothing is written
	// to the instance's state, its file is not read or removed, and a
	// declaration that cannot run is an error rather than something to wait
	// out.
	Test bool
	// MonitorState is the monitor's own state directory. Empty is the
	// instance's, under Dirs.
	MonitorState string
	// Stderr is where the monitor's stderr goes. Nil is the instance's
	// rotating log.
	Stderr io.Writer

	status Status
}

// Run runs the instance until ctx ends, it is disabled, or it ends itself.
func (r *Runner) Run(ctx context.Context) error {
	if r.Emit == nil {
		queue := r.Dirs.Queue()
		r.Emit = func(ev Event) error {
			_, err := queue.Add(ev)
			return err
		}
	}
	if r.MonitorState == "" {
		r.MonitorState = r.Dirs.MonitorStateDir(r.Instance.Name, r.Instance.ID)
	}
	if err := os.MkdirAll(r.MonitorState, 0o755); err != nil {
		return err
	}
	if r.Stderr == nil {
		log := &rotatingLog{path: filepath.Join(r.Dirs.InstanceStateDir(r.Instance.Name), logFile)}
		defer log.Close()
		r.Stderr = log
	}
	if !r.Test {
		// A runner restarted under the same instance carries on its counts,
		// and whether it was failing, so recovery is still reported once.
		if previous, ok, _ := ReadStatus(r.Dirs, r.Instance.Name); ok {
			r.status = previous
		}
	}
	r.status.Instance, r.status.Monitor = r.Instance.Name, r.Instance.Monitor
	r.setState(StateStarting, nil)

	backoff := time.Duration(0)
	for {
		if !r.Test && !r.stillEnabled() {
			return nil
		}
		def, err := Load(r.Instance.Path, r.Instance.Builtin)
		if err == nil && def.Problem != "" {
			err = errors.New(def.Problem)
		}
		var interval time.Duration
		if err == nil {
			// The declaration may have changed what an interval it takes.
			interval, err = def.IntervalFor(r.Instance.Options)
		}
		if err != nil {
			if r.Test {
				return err
			}
			r.failing(fmt.Sprintf("cannot run %s: %v", r.Instance.Path, err), "")
			r.setState(StateFailed, nil)
			if sleep(ctx, brokenRecheck) != nil {
				return nil
			}
			continue
		}

		started := time.Now()
		final, tail, runErr := r.runOnce(ctx, def)
		if ctx.Err() != nil {
			return nil
		}
		if runErr != nil {
			r.failing(runErr.Error(), tail)
		} else {
			r.recovered()
		}
		if final || (interval == 0 && runErr == nil) {
			r.setState(StateDone, nil)
			if !r.Test {
				return finish(r.Dirs, r.Instance.Name)
			}
			return nil
		}

		var wait time.Duration
		if interval > 0 {
			wait = time.Until(started.Add(interval))
			next := time.Now().Add(wait)
			r.setState(StateWaiting, &next)
		} else {
			if time.Since(started) >= stableRun {
				backoff = 0
			}
			backoff = min(max(2*backoff, time.Second), maxBackoff)
			wait = backoff
			next := time.Now().Add(wait)
			r.setState(StateBackoff, &next)
		}
		if sleep(ctx, wait) != nil {
			return nil
		}
	}
}

// runOnce runs the monitor's script once, to its end, queuing what it prints.
// final reports a final event; tail is the end of what it wrote to stderr.
func (r *Runner) runOnce(ctx context.Context, def Definition) (final bool, tail string, err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:gosec // G204: the monitor the repository or image declared is what is meant to run.
	cmd := exec.CommandContext(runCtx, def.Path)
	cmd.Dir = r.Instance.Workdir
	cmd.Env = r.environ()
	ownProcessGroup(cmd)
	// A child that left the group can still hold the pipes open after the
	// script exits; that must not hold up the run.
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false, "", err
	}
	stderr := &tailWriter{max: stderrTail}
	cmd.Stderr = io.MultiWriter(r.Stderr, stderr)

	now := time.Now()
	r.status.Runs++
	r.status.LastRunAt = &now
	r.setState(StateRunning, nil)
	if err := cmd.Start(); err != nil {
		return false, "", err
	}

	reader := bufio.NewReaderSize(stdout, 64<<10)
	var emitErr error
	for {
		raw, readErr := readLine(reader)
		if ev, ok := ParseLine(raw); ok && emitErr == nil {
			ev.Instance, ev.Monitor = r.Instance.Name, r.Instance.Monitor
			if emitErr = r.Emit(ev); emitErr == nil {
				at := time.Now()
				r.status.Events++
				r.status.LastEventAt = &at
				r.writeStatus()
			}
			if ev.Final {
				final = true
				// Nothing after a final event is wanted; a streaming
				// monitor would otherwise run on.
				cancel()
			}
		}
		if readErr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	tail = stderr.String()
	switch {
	case emitErr != nil:
		return final, tail, fmt.Errorf("queue an event: %w", emitErr)
	case final:
		// Ended by its final event, however the process took being ended.
		return true, tail, nil
	case waitErr != nil:
		return false, tail, waitErr
	}
	return false, tail, nil
}

// The variables a monitor is told its instance in, besides each option's own
// (Option.EnvName).
const (
	EnvName  = "DISCOBOX_MONITOR_NAME"
	EnvState = "DISCOBOX_MONITOR_STATE"
	// EnvOptions is every option, as a JSON object.
	EnvOptions = "DISCOBOX_MONITOR_OPTIONS"
)

// environ is the monitor's environment: the runner's own, less any monitor
// variables it inherited, plus this instance's.
func (r *Runner) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "DISCOBOX_OPT_") || strings.HasPrefix(kv, "DISCOBOX_MONITOR_") {
			continue
		}
		env = append(env, kv)
	}
	options := "{}"
	if len(r.Instance.Options) > 0 {
		if data, err := json.Marshal(r.Instance.Options); err == nil {
			options = string(data)
		}
	}
	env = append(env,
		EnvName+"="+r.Instance.Name,
		EnvState+"="+r.MonitorState,
		EnvOptions+"="+options,
	)
	for key, value := range r.Instance.Options {
		env = append(env, OptionEnvName(key)+"="+value)
	}
	return env
}

// failing records a failed run, and queues monitor.failed when the instance
// was not already failing: a monitor failing every minute is one event, not
// one a minute.
func (r *Runner) failing(reason, tail string) {
	wasFailing := r.status.Failing
	r.status.Failing = true
	r.status.LastError = reason
	if last := lastLine(tail); last != "" {
		r.status.LastError = reason + ": " + last
	}
	r.writeStatus()
	if wasFailing {
		return
	}
	_ = r.Emit(Event{
		Instance: r.Instance.Name,
		Monitor:  r.Instance.Monitor,
		Kind:     KindFailed,
		Key:      KindFailed,
		Title:    fmt.Sprintf("%s (%s) is failing: %s", r.Instance.Name, r.Instance.Monitor, r.status.LastError),
		Summary:  "discobox-monitor logs " + r.Instance.Name + " for its stderr",
		Body:     tail,
	})
}

func (r *Runner) recovered() {
	if !r.status.Failing {
		return
	}
	r.status.Failing = false
	r.status.LastError = ""
	r.writeStatus()
	_ = r.Emit(Event{
		Instance: r.Instance.Name,
		Monitor:  r.Instance.Monitor,
		Kind:     KindRecovered,
		Key:      KindFailed,
		Title:    fmt.Sprintf("%s (%s) is working again", r.Instance.Name, r.Instance.Monitor),
	})
}

func (r *Runner) setState(state State, next *time.Time) {
	r.status.State = state
	r.status.NextRunAt = next
	r.writeStatus()
}

func (r *Runner) writeStatus() {
	if r.Test {
		return
	}
	r.status.UpdatedAt = time.Now().UTC()
	_ = writeJSONFile(filepath.Join(r.Dirs.InstanceStateDir(r.Instance.Name), statusFile), r.status)
}

// stillEnabled reports whether the instance's file is still this instance:
// one disabled and enabled again under its name is another, with its own
// runner to come.
func (r *Runner) stillEnabled() bool {
	current, err := ReadInstance(r.Dirs, r.Instance.Name)
	return err == nil && current.ID == r.Instance.ID
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// readLine is one line, without its newline, of at most maxLine bytes; the
// rest of a longer one is read and dropped. The error is the reader's, with
// the last line returned beside it.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf) < maxLine {
			buf = append(buf, chunk[:min(len(chunk), maxLine-len(buf))]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return toValidUTF8(string(t.buf))
}

// rotatingLog appends to path, moving it to path.1 once it reaches maxLog.
//
// It never fails a write: it sits on the monitor's stderr, and a log that
// cannot be written must not become a broken pipe the monitor dies of.
type rotatingLog struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func (l *rotatingLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.write(p)
	return len(p), nil
}

func (l *rotatingLog) write(p []byte) error {
	if l.file != nil && l.size+int64(len(p)) > maxLog {
		l.file.Close()
		l.file = nil
		_ = os.Rename(l.path, l.path+".1")
	}
	if l.file == nil {
		if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
			return err
		}
		file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return err
		}
		l.file, l.size = file, info.Size()
	}
	n, err := l.file.Write(p)
	l.size += int64(n)
	return err
}

func (l *rotatingLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
