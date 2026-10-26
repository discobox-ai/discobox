package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/harnesscapture"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) GetSandboxHarnessConfigChanges(ctx context.Context, p serverapi.GetSandboxHarnessConfigChangesParams) (serverapi.GetSandboxHarnessConfigChangesRes, error) {
	out, err := h.services.HarnessConfigs.GetSandboxHarnessConfigChanges(ctx, p.ProjectId, p.SandboxId)
	if err != nil {
		return &serverapi.ErrorResponseStatusCode{StatusCode: statusCodeForError(err), Response: serverapi.ErrorResponse{Error: err.Error()}}, nil
	}
	body, err := services.Convert[serverapi.HarnessConfigChanges](out)
	return &body, err
}
func (h *Handler) SaveSandboxHarnessConfigChanges(ctx context.Context, r *serverapi.HarnessConfigCandidateRequest, p serverapi.SaveSandboxHarnessConfigChangesParams) (serverapi.SaveSandboxHarnessConfigChangesRes, error) {
	input, err := services.Convert[harnesscapture.CandidateRequest](r)
	if err != nil {
		return nil, err
	}
	out, err := h.services.HarnessConfigs.SaveSandboxHarnessConfigChanges(ctx, p.ProjectId, p.SandboxId, input)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.HarnessConfigSaveResult](out)
	return &body, err
}
func (h *Handler) ReconsiderSandboxHarnessConfigChanges(ctx context.Context, r *serverapi.HarnessConfigSelection, p serverapi.ReconsiderSandboxHarnessConfigChangesParams) (serverapi.ReconsiderSandboxHarnessConfigChangesRes, error) {
	input, err := services.Convert[harnesscapture.Selection](r)
	if err != nil {
		return nil, err
	}
	out, err := h.services.HarnessConfigs.ReconsiderSandboxHarnessConfigChanges(ctx, p.ProjectId, p.SandboxId, input)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[serverapi.HarnessConfigChanges](out)
	return &body, err
}
func (h *Handler) DiscardSandboxHarnessConfigChanges(ctx context.Context, r *serverapi.HarnessConfigCandidateRequest, p serverapi.DiscardSandboxHarnessConfigChangesParams) (serverapi.DiscardSandboxHarnessConfigChangesRes, error) {
	input, err := services.Convert[harnesscapture.CandidateRequest](r)
	if err != nil {
		return &serverapi.ErrorResponseStatusCode{StatusCode: statusCodeForError(err), Response: serverapi.ErrorResponse{Error: err.Error()}}, nil
	}
	out, err := h.services.HarnessConfigs.DiscardSandboxHarnessConfigChanges(ctx, p.ProjectId, p.SandboxId, input)
	if err != nil {
		return &serverapi.ErrorResponseStatusCode{StatusCode: statusCodeForError(err), Response: serverapi.ErrorResponse{Error: err.Error()}}, nil
	}
	body, err := services.Convert[serverapi.HarnessConfigChanges](out)
	return &body, err
}

// CaptureSandboxHarnessConfig is private to the agent. The control plane
// originates them only after a user asks to save, and never exposes a proxy.
func (h *Handler) CaptureSandboxHarnessConfig(context.Context, *serverapi.HarnessConfigCandidateRequest, serverapi.CaptureSandboxHarnessConfigParams) (serverapi.CaptureSandboxHarnessConfigRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}
func (h *Handler) AcknowledgeSandboxHarnessConfig(context.Context, *serverapi.HarnessConfigAcknowledgment, serverapi.AcknowledgeSandboxHarnessConfigParams) (serverapi.AcknowledgeSandboxHarnessConfigRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}
func (h *Handler) ReconsiderSandboxHarnessConfig(context.Context, *serverapi.HarnessConfigReconsideration, serverapi.ReconsiderSandboxHarnessConfigParams) (serverapi.ReconsiderSandboxHarnessConfigRes, error) {
	return sandboxAgentRuntimeNotImplemented(), nil
}
