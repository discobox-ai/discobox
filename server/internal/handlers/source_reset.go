package handlers

import (
	"context"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/server/internal/model"
)

func (h *Handler) GetSandboxSourceReset(ctx context.Context, params api.GetSandboxSourceResetParams) (api.GetSandboxSourceResetRes, error) {
	state, err := h.services.Sandboxes.GetSandboxSourceReset(ctx, params.ProjectId, params.SandboxId, params.Slug)
	if err != nil {
		return apiError(err), nil
	}
	return sourceResetStatus(state), nil
}

func (h *Handler) PostSandboxSourceReset(ctx context.Context, req *api.SandboxSourceResetRequest, params api.PostSandboxSourceResetParams) (api.PostSandboxSourceResetRes, error) {
	state, err := h.services.Sandboxes.PostSandboxSourceReset(ctx, params.ProjectId, params.SandboxId, params.Slug, req.ExpectedRevision, req.Commit)
	if err != nil {
		return apiError(err), nil
	}
	return sourceResetStatus(state), nil
}

func sourceResetStatus(state model.SandboxSourceReset) *api.SandboxSourceResetStatus {
	result := &api.SandboxSourceResetStatus{Revision: state.Revision, AppliedRevision: state.AppliedRevision}
	if state.Commit != "" {
		result.Commit.SetTo(state.Commit)
	}
	return result
}
