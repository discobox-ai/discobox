package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/shimproxy"
	"github.com/discobox-ai/discobox/sandbox-agent/shimruntime"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
)

// A terminal read, typed into, and waited on without attaching (ADR 0137).

// maxTerminalWait bounds one wait; a caller waiting longer asks again with the
// resume point it was last given (ADR 0137 §3).
const maxTerminalWait = 60 * time.Second

func (h *handler) GetSandboxExecScreen(ctx context.Context, params sandboxapi.GetSandboxExecScreenParams) (*sandboxapi.SandboxExecScreen, error) {
	execID, err := h.resolveExecIDReadOnly(params.ExecId)
	if err != nil {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	screen, err := h.execs.Screen(ctx, execID, int(params.Scrollback.Or(0)))
	if err != nil {
		return nil, terminalError(err)
	}
	out := &sandboxapi.SandboxExecScreen{
		Rows:          int64(screen.Rows),
		Cols:          int64(screen.Cols),
		CursorRow:     int64(screen.CursorRow),
		CursorCol:     int64(screen.CursorCol),
		CursorVisible: screen.CursorVisible,
		AltScreen:     screen.AltScreen,
		Exited:        screen.Exited,
		Lines:         screen.Lines,
		Scrollback:    screen.Scrollback,
	}
	if screen.Title != "" {
		out.Title = sandboxapi.NewOptString(screen.Title)
	}
	if screen.OutputAt != nil {
		out.OutputAt = sandboxapi.NewOptDateTime(*screen.OutputAt)
	}
	return out, nil
}

func (h *handler) SendSandboxExecInput(ctx context.Context, req *sandboxapi.SandboxExecInputBody, params sandboxapi.SendSandboxExecInputParams) (*sandboxapi.SandboxExecInputResult, error) {
	execID, err := h.resolveExecIDReadOnly(params.ExecId)
	if err != nil {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	parts := make([]shimruntime.InputPart, 0, len(req.Input))
	for _, part := range req.Input {
		parts = append(parts, shimruntime.InputPart{Text: part.Text.Or(""), Key: part.Key.Or("")})
	}
	// Taken before the input is delivered, so a wait resuming from it finds a
	// hook the input causes however soon after it is recorded.
	resumeAfter := time.Now().UTC()
	if err := h.execs.Input(ctx, execID, parts); err != nil {
		return nil, terminalError(err)
	}
	return &sandboxapi.SandboxExecInputResult{ResumeAfter: formatResumePoint(resumeAfter)}, nil
}

func (h *handler) WaitSandboxExec(ctx context.Context, req *sandboxapi.SandboxExecWaitBody, params sandboxapi.WaitSandboxExecParams) (*sandboxapi.SandboxExecWaitResult, error) {
	execID, err := h.resolveExecIDReadOnly(params.ExecId)
	if err != nil {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}
	until := req.Until
	hookEvents := until.HookEvents
	quiet := int(until.QuietSeconds.Or(0))
	exit := until.Exit.Or(false)
	if len(hookEvents) == 0 && quiet == 0 && !exit {
		return nil, statusError{status: http.StatusBadRequest, message: "a wait needs something to wait for: hookEvents, quietSeconds, or exit"}
	}
	if len(hookEvents) > 0 && h.store == nil {
		return nil, statusError{status: http.StatusServiceUnavailable, message: "this sandbox agent keeps no harness hook record"}
	}
	timeout := min(time.Duration(req.TimeoutSeconds)*time.Second, maxTerminalWait)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Where hooks count from: the resume point a previous call answered with,
	// or the start of this one. A wait that ends on anything but a hook answers
	// with this same point, so the next resumes where this one began and a hook
	// recorded while it returned is not lost between them.
	since := time.Now().UTC()
	if after := until.After.Or(""); after != "" {
		if since, err = parseResumePoint(after); err != nil {
			return nil, statusError{status: http.StatusBadRequest, message: "after is not a resume point this agent answered with"}
		}
	}

	// The shim waits on what only it sees — output and exit — while this waits
	// on the hooks the collector records. Whichever holds first answers.
	type shimAnswer struct {
		result execs.ShimWaitResult
		err    error
	}
	shimDone := make(chan shimAnswer, 1)
	if quiet > 0 || exit {
		go func() {
			result, err := h.execs.AwaitTerminal(ctx, execID, execs.ShimWait{QuietSeconds: quiet, Exit: exit, TimeoutSeconds: int(timeout / time.Second)})
			shimDone <- shimAnswer{result, err}
		}()
	} else if _, ok := h.execs.Get(execID); !ok {
		return nil, statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	}

	for {
		// Taken before the read, so a hook recorded between the read and the
		// select still wakes this. Waiting on no hooks, it stays nil and never
		// fires.
		var signal <-chan struct{}
		if len(hookEvents) > 0 {
			signal = h.store.HarnessHookSignal()
			hook, err := h.store.FirstHarnessHookSince(ctx, execID, since, hookEvents)
			if err != nil && ctx.Err() == nil {
				return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
			}
			if hook != nil {
				return h.waitResult(ctx, execID, "hook", hook, since), nil
			}
		}
		select {
		case answer := <-shimDone:
			if answer.err != nil {
				if ctx.Err() != nil {
					return h.waitResult(ctx, execID, shimruntime.WaitTimeout, nil, since), nil
				}
				if errors.Is(answer.err, execs.ErrSessionGone) && exit {
					return h.waitResult(ctx, execID, shimruntime.WaitExit, nil, since), nil
				}
				return nil, terminalError(answer.err)
			}
			out := &sandboxapi.SandboxExecWaitResult{Reason: sandboxapi.SandboxExecWaitResultReason(answer.result.Reason), ResumeAfter: formatResumePoint(since)}
			if answer.result.OutputAt != nil {
				out.OutputAt = sandboxapi.NewOptDateTime(*answer.result.OutputAt)
			}
			return out, nil
		case <-signal:
		case <-ctx.Done():
			return h.waitResult(context.Background(), execID, shimruntime.WaitTimeout, nil, since), nil
		}
	}
}

// waitResult reports what a wait ended on, with the terminal's last output
// time when its screen can still be read, and where the next wait resumes:
// after the hook it ended on, or from since, where this one began.
func (h *handler) waitResult(ctx context.Context, execID, reason string, hook *store.HarnessHookRecord, since time.Time) *sandboxapi.SandboxExecWaitResult {
	out := &sandboxapi.SandboxExecWaitResult{Reason: sandboxapi.SandboxExecWaitResultReason(reason), ResumeAfter: formatResumePoint(since)}
	if hook != nil {
		out.Hook = sandboxapi.NewOptHarnessHookLog(harnessHookLog(*hook))
		out.ResumeAfter = formatResumePoint(hook.CreatedAt)
	}
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if screen, err := h.execs.Screen(readCtx, execID, 0); err == nil && screen.OutputAt != nil {
		out.OutputAt = sandboxapi.NewOptDateTime(*screen.OutputAt)
	}
	return out
}

// terminalError maps a terminal call's failure onto the status it means.
func terminalError(err error) error {
	var shimErr *shimproxy.ShimError
	switch {
	case errors.Is(err, execs.ErrNotFound):
		return statusError{status: http.StatusNotFound, message: "sandbox exec not found"}
	case errors.Is(err, execs.ErrSessionGone):
		return statusError{status: http.StatusConflict, message: "sandbox exec has ended"}
	case errors.As(err, &shimErr):
		return statusError{status: shimErr.Status, message: shimErr.Message}
	default:
		return statusError{status: http.StatusBadGateway, message: err.Error()}
	}
}

// A resume point is where a terminal wait counts hooks from: the recorded time
// a hook came after, at the precision the hook record keeps. It travels as an
// opaque string rather than a date-time because an API date-time is whole
// seconds, and a point rounded down to its second finds the hook it resumes
// after again.
func formatResumePoint(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func parseResumePoint(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
