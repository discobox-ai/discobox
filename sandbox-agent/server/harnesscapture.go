package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	hc "github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/sandbox-agent/terminal"
)

func captureConvert[T any](value any) (*T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out T
	if err = json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
func captureError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, terminal.ErrCaptureDefinitionChanged) {
		return statusError{status: http.StatusPreconditionFailed, message: err.Error()}
	}
	if errors.Is(err, terminal.ErrCaptureConflict) {
		return statusError{status: http.StatusConflict, message: err.Error()}
	}
	return statusError{status: http.StatusServiceUnavailable, message: "harness configuration capture unavailable"}
}
func capturePreview(p hc.Preview, err error) (*sandboxapi.HarnessConfigChanges, error) {
	if err != nil {
		return nil, captureError(err)
	}
	return captureConvert[sandboxapi.HarnessConfigChanges](p)
}
func (h *handler) GetSandboxHarnessConfigChanges(ctx context.Context, _ sandboxapi.GetSandboxHarnessConfigChangesParams) (*sandboxapi.HarnessConfigChanges, error) {
	if h.terminals == nil {
		return capturePreview(hc.Preview{State: "unsupported"}, nil)
	}
	return capturePreview(h.terminals.HarnessConfigChanges(ctx))
}
func (h *handler) DiscardSandboxHarnessConfigChanges(ctx context.Context, req *sandboxapi.HarnessConfigCandidateRequest, _ sandboxapi.DiscardSandboxHarnessConfigChangesParams) (*sandboxapi.HarnessConfigChanges, error) {
	if h.terminals == nil {
		return nil, captureError(terminal.ErrCaptureUnavailable)
	}
	return capturePreview(h.terminals.DiscardHarnessConfig(ctx, req.CandidateId))
}
func (h *handler) CaptureSandboxHarnessConfig(ctx context.Context, req *sandboxapi.HarnessConfigCandidateRequest, _ sandboxapi.CaptureSandboxHarnessConfigParams) (*sandboxapi.HarnessConfigCandidate, error) {
	if h.terminals == nil {
		return nil, captureError(terminal.ErrCaptureUnavailable)
	}
	c, err := h.terminals.CaptureHarnessConfig(ctx, req.CandidateId)
	if err != nil {
		return nil, captureError(err)
	}
	return captureConvert[sandboxapi.HarnessConfigCandidate](c)
}
func (h *handler) AcknowledgeSandboxHarnessConfig(ctx context.Context, req *sandboxapi.HarnessConfigAcknowledgment, _ sandboxapi.AcknowledgeSandboxHarnessConfigParams) (*sandboxapi.HarnessConfigChanges, error) {
	if h.terminals == nil {
		return nil, captureError(terminal.ErrCaptureUnavailable)
	}
	a, err := captureConvert[hc.Acknowledgment](req)
	if err != nil {
		return nil, captureError(err)
	}
	return capturePreview(h.terminals.AcknowledgeHarnessConfig(ctx, *a))
}
func (h *handler) ReconsiderSandboxHarnessConfig(ctx context.Context, req *sandboxapi.HarnessConfigReconsideration, _ sandboxapi.ReconsiderSandboxHarnessConfigParams) (*sandboxapi.HarnessConfigChanges, error) {
	if h.terminals == nil {
		return nil, captureError(terminal.ErrCaptureUnavailable)
	}
	r, err := captureConvert[hc.Reconsideration](req)
	if err != nil {
		return nil, captureError(err)
	}
	return capturePreview(h.terminals.ReconsiderHarnessConfig(ctx, *r))
}
