package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) GetSandboxSkills(ctx context.Context, params serverapi.GetSandboxSkillsParams) (serverapi.GetSandboxSkillsRes, error) {
	current, err := h.services.Sandboxes.GetSandboxSkills(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	return sandboxSkillsInventory(current)
}

func (h *Handler) MutateSandboxSkills(ctx context.Context, req *serverapi.SandboxSkillMutation, params serverapi.MutateSandboxSkillsParams) (serverapi.MutateSandboxSkillsRes, error) {
	change, err := services.Convert[sandboxconfig.SkillMutation](req)
	if err != nil {
		return nil, err
	}
	current, err := h.services.Sandboxes.MutateSandboxSkills(ctx, params.ProjectId, params.SandboxId, change)
	if err != nil {
		return apiError(err), nil
	}
	return sandboxSkillsInventory(current)
}

func sandboxSkillsInventory(current model.SandboxManagedSkills) (*serverapi.SandboxSkillsInventory, error) {
	out, err := services.Convert[serverapi.SandboxSkillsInventory](struct {
		DesiredRevision int64                                 `json:"desiredRevision"`
		AppliedRevision int64                                 `json:"appliedRevision"`
		Installed       map[string]sandboxconfig.SkillVersion `json:"installed,omitempty"`
		Application     string                                `json:"application,omitempty"`
		Desired         map[string]sandboxconfig.SkillVersion `json:"desired,omitempty"`
		Sources         map[string]sandboxconfig.SkillOrigin  `json:"sources,omitempty"`
	}{current.Desired.Revision, current.Applied, current.Installed, current.Application, current.Desired.Versions, current.Desired.Sources})
	return &out, err
}

func (h *Handler) GetPoolSandboxRuntimeIntent(ctx context.Context, params serverapi.GetPoolSandboxRuntimeIntentParams) (serverapi.GetPoolSandboxRuntimeIntentRes, error) {
	current, err := h.services.Pools.GetPoolSandboxRuntimeIntent(ctx, params.PoolId, params.SandboxId, params.KnownRevision.Or(0))
	if err != nil {
		return apiError(err), nil
	}
	if current == nil {
		return &serverapi.GetPoolSandboxRuntimeIntentNotModified{}, nil
	}
	response, err := services.Convert[serverapi.PoolSandboxRuntimeIntent](current)
	return &response, err
}
