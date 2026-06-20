package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sessions "github.com/obot-platform/discobox/sessions"
	sessionapigen "github.com/obot-platform/discobox/sessions/api/gen"
	"gorm.io/gorm"
)

type generatedHandler struct {
	r *runtimeState
}

func (h *generatedHandler) SessionsAgents(ctx context.Context) (*sessionapigen.AgentsResponse, error) {
	return convertToGenerated[sessionapigen.AgentsResponse](map[string]any{"agents": h.r.agents})
}

func (h *generatedHandler) SessionsAttach(context.Context, sessionapigen.SessionsAttachParams) (sessionapigen.SessionsAttachRes, error) {
	return &sessionapigen.SessionsAttachSwitchingProtocols{}, nil
}

func (h *generatedHandler) SessionsCreate(ctx context.Context, req *sessionapigen.CreateRequest) (sessionapigen.SessionsCreateRes, error) {
	var body sessions.CreateRequest
	if err := convertGenerated(req, &body); err != nil {
		return nil, err
	}
	session, err := h.r.create(ctx, body)
	if err != nil {
		return createBadRequest(err), nil
	}
	return convertToGenerated[sessionapigen.CreateResponse](map[string]any{"session": session})
}

func (h *generatedHandler) SessionsList(ctx context.Context) (*sessionapigen.SessionsResponse, error) {
	if err := h.r.reconcileSessions(ctx); err != nil {
		return nil, err
	}
	list, err := h.r.listSessions(ctx)
	if err != nil {
		return nil, err
	}
	return convertToGenerated[sessionapigen.SessionsResponse](map[string]any{"sessions": list})
}

func (h *generatedHandler) SessionsPing(context.Context) (*sessionapigen.PingResponse, error) {
	return &sessionapigen.PingResponse{SessionId: h.r.cfg.SessionID, RepoRoot: h.r.cfg.RepoRoot, Version: h.r.cfg.Version}, nil
}

func (h *generatedHandler) SessionsResize(ctx context.Context, req *sessionapigen.ResizeRequest, params sessionapigen.SessionsResizeParams) (sessionapigen.SessionsResizeRes, error) {
	if err := h.r.supervisorResize(ctx, params.SessionId, sessionsResizeRequest(req)); err != nil {
		return resizeError(err), nil
	}
	return &sessionapigen.ActionResponse{Resized: sessionapigen.NewOptBool(true)}, nil
}

func (h *generatedHandler) SessionsShutdown(context.Context) (*sessionapigen.ShutdownResponse, error) {
	go h.r.cancel()
	return &sessionapigen.ShutdownResponse{Shutdown: true}, nil
}

func (h *generatedHandler) SessionsSignal(ctx context.Context, req *sessionapigen.SignalRequest, params sessionapigen.SessionsSignalParams) (sessionapigen.SessionsSignalRes, error) {
	if err := h.r.supervisorSignal(ctx, params.SessionId, req.Signal); err != nil {
		return signalError(err), nil
	}
	return &sessionapigen.ActionResponse{Signaled: sessionapigen.NewOptBool(true)}, nil
}

func (h *generatedHandler) SessionsStatus(ctx context.Context) (*sessionapigen.StatusResponse, error) {
	if err := h.r.reconcileSessions(ctx); err != nil {
		return nil, err
	}
	list, err := h.r.listSessions(ctx)
	if err != nil {
		return nil, err
	}
	return convertToGenerated[sessionapigen.StatusResponse](map[string]any{
		"sessionId": h.r.cfg.SessionID,
		"repoRoot":  h.r.cfg.RepoRoot,
		"version":   h.r.cfg.Version,
		"sessions":  list,
	})
}

func createBadRequest(err error) sessionapigen.SessionsCreateRes {
	return &sessionapigen.ErrorResponse{Error: err.Error()}
}

func sessionsResizeRequest(req *sessionapigen.ResizeRequest) sessions.ResizeRequest {
	return sessions.ResizeRequest{Cols: uint16(req.Cols), Rows: uint16(req.Rows)}
}

func resizeError(err error) sessionapigen.SessionsResizeRes {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return (*sessionapigen.SessionsResizeNotFound)(&sessionapigen.ErrorResponse{Error: err.Error()})
	}
	return (*sessionapigen.SessionsResizeBadRequest)(&sessionapigen.ErrorResponse{Error: err.Error()})
}

func signalError(err error) sessionapigen.SessionsSignalRes {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return (*sessionapigen.SessionsSignalNotFound)(&sessionapigen.ErrorResponse{Error: err.Error()})
	}
	return (*sessionapigen.SessionsSignalBadRequest)(&sessionapigen.ErrorResponse{Error: err.Error()})
}

func convertGenerated(in any, out any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("convert generated value: %w", err)
	}
	return nil
}

func convertToGenerated[T any](in any) (*T, error) {
	var out T
	if err := convertGenerated(in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
