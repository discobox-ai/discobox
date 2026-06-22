package daemon

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"

	"github.com/creack/pty"
	supervisorapigen "github.com/obot-platform/discobox/sessions/api/supervisorgen"
	"github.com/ogen-go/ogen/ogenerrors"
)

type supervisorGeneratedHandler struct {
	r *supervisorRuntime
}

func (h *supervisorGeneratedHandler) SupervisorAttach(context.Context) (supervisorapigen.SupervisorAttachRes, error) {
	return &supervisorapigen.SupervisorAttachSwitchingProtocols{}, nil
}

func (h *supervisorGeneratedHandler) SupervisorResize(_ context.Context, req *supervisorapigen.ResizeRequest) (supervisorapigen.SupervisorResizeRes, error) {
	if msg := h.resize(req); msg != "" {
		resp := supervisorapigen.SupervisorResizeBadRequest(supervisorapigen.ErrorResponse{Error: msg})
		return &resp, nil
	}
	return &supervisorapigen.ActionResponse{Resized: supervisorapigen.NewOptBool(true)}, nil
}

func (h *supervisorGeneratedHandler) SupervisorSignal(_ context.Context, req *supervisorapigen.SignalRequest) (supervisorapigen.SupervisorSignalRes, error) {
	if msg := h.signal(req); msg != "" {
		resp := supervisorapigen.SupervisorSignalBadRequest(supervisorapigen.ErrorResponse{Error: msg})
		return &resp, nil
	}
	return &supervisorapigen.ActionResponse{Signaled: supervisorapigen.NewOptBool(true)}, nil
}

func (h *supervisorGeneratedHandler) SupervisorStatus(context.Context) (supervisorapigen.SupervisorStatusRes, error) {
	h.r.mu.Lock()
	exit := h.r.exit
	h.r.mu.Unlock()
	status := h.r.cfg.Session
	if h.r.cmd != nil && h.r.cmd.Process != nil {
		status.PID = h.r.cmd.Process.Pid
		status.Running = exit.ExitedAt == nil
	}
	status.ExitCode = exit.ExitCode
	status.Error = exit.Error
	status.ExitedAt = exit.ExitedAt
	return convertToGenerated[supervisorapigen.Session](status)
}

type supervisorSecurityHandler struct {
	token string
}

func (h supervisorSecurityHandler) HandleBearerAuth(ctx context.Context, _ supervisorapigen.OperationName, auth supervisorapigen.BearerAuth) (context.Context, error) {
	if subtle.ConstantTimeCompare([]byte(auth.Token), []byte(h.token)) != 1 {
		return ctx, fmt.Errorf("unauthorized")
	}
	return ctx, nil
}

type supervisorSecuritySource struct {
	token string
}

func (s supervisorSecuritySource) BearerAuth(context.Context, supervisorapigen.OperationName) (supervisorapigen.BearerAuth, error) {
	return supervisorapigen.BearerAuth{Token: s.token}, nil
}

func supervisorErrorHandler(_ context.Context, w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, ogenerrors.ErrorCode(err), err)
}

func (h *supervisorGeneratedHandler) resize(req *supervisorapigen.ResizeRequest) string {
	if err := pty.Setsize(h.r.tty, &pty.Winsize{Rows: resizeDimension(req.Rows), Cols: resizeDimension(req.Cols)}); err != nil {
		return err.Error()
	}
	return ""
}

func (h *supervisorGeneratedHandler) signal(req *supervisorapigen.SignalRequest) string {
	if err := signalProcess(h.r.cmd.Process, req.Signal); err != nil {
		return err.Error()
	}
	return ""
}
