package handlers

import (
	"context"
	"strings"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListSandboxes(ctx context.Context, params serverapi.ListSandboxesParams) (serverapi.ListSandboxesRes, error) {
	sandboxes, err := h.services.Sandboxes.ListSandboxes(ctx, params.ProjectId, strings.TrimSpace(params.SourceRoot.Or("")), strings.TrimSpace(params.OriginKey.Or("")))
	if err != nil {
		return apiError(err), nil
	}
	converted, err := services.SandboxesToAPI(sandboxes, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	body, err := services.Convert[apimodel.ListSandboxesBody](struct {
		Sandboxes any `json:"sandboxes"`
	}{Sandboxes: converted})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSandbox(ctx context.Context, req *apimodel.CreateSandboxBody, params serverapi.CreateSandboxParams) (serverapi.CreateSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.CreateSandbox(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetSandbox(ctx context.Context, params serverapi.GetSandboxParams) (serverapi.GetSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.GetSandbox(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpdateSandbox(ctx context.Context, req *apimodel.UpdateSandboxBody, params serverapi.UpdateSandboxParams) (serverapi.UpdateSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.UpdateSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSandbox(ctx context.Context, params serverapi.DeleteSandboxParams) (serverapi.DeleteSandboxRes, error) {
	if err := h.services.Sandboxes.DeleteSandbox(ctx, params.ProjectId, params.SandboxId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSandboxAccepted{}, nil
}

func (h *Handler) UnarchiveSandbox(ctx context.Context, params serverapi.UnarchiveSandboxParams) (serverapi.UnarchiveSandboxRes, error) {
	if err := h.services.Sandboxes.UnarchiveSandbox(ctx, params.ProjectId, params.SandboxId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.UnarchiveSandboxAccepted{}, nil
}

// PurgeSandbox holds the request open until the removal is confirmed, unlike
// every other existence change here (ADR 0022 §3).
func (h *Handler) PurgeSandbox(ctx context.Context, params serverapi.PurgeSandboxParams) (serverapi.PurgeSandboxRes, error) {
	if err := h.services.Sandboxes.PurgeSandbox(ctx, params.ProjectId, params.SandboxId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.PurgeSandboxNoContent{}, nil
}

func (h *Handler) StartSandbox(ctx context.Context, req *apimodel.StartSandboxBody, params serverapi.StartSandboxParams) (serverapi.StartSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.StartSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) StopSandbox(ctx context.Context, req *apimodel.StopSandboxBody, params serverapi.StopSandboxParams) (serverapi.StopSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.StopSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RestartSandbox(ctx context.Context, req *apimodel.RestartSandboxBody, params serverapi.RestartSandboxParams) (serverapi.RestartSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.RestartSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) RepairSandbox(ctx context.Context, params serverapi.RepairSandboxParams) (serverapi.RepairSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.RepairSandbox(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) UpgradeSandbox(ctx context.Context, req *apimodel.UpgradeSandboxBody, params serverapi.UpgradeSandboxParams) (serverapi.UpgradeSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.UpgradeSandbox(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CompleteSandboxSourcePush(ctx context.Context, req *apimodel.CompleteSandboxSourcePushBody, params serverapi.CompleteSandboxSourcePushParams) (serverapi.CompleteSandboxSourcePushRes, error) {
	sandbox, err := h.services.Sandboxes.CompleteSandboxSourcePush(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CompleteSandboxApply(ctx context.Context, req *apimodel.CompleteSandboxApplyBody, params serverapi.CompleteSandboxApplyParams) (serverapi.CompleteSandboxApplyRes, error) {
	sandbox, err := h.services.Sandboxes.CompleteSandboxApply(ctx, params.ProjectId, params.SandboxId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) AssignSandboxHarnessSecrets(ctx context.Context, req *apimodel.AssignSandboxHarnessSecretsBody, params serverapi.AssignSandboxHarnessSecretsParams) (serverapi.AssignSandboxHarnessSecretsRes, error) {
	secrets, err := h.services.Sandboxes.AssignSandboxHarnessSecrets(ctx, params.ProjectId, params.SandboxId, req.HarnessConfigId)
	if err != nil {
		return apiError(err), nil
	}
	resp := apimodel.SandboxHarnessSecretsResponse{Secrets: secrets}
	return &resp, nil
}

func (h *Handler) ReconcileSandbox(ctx context.Context, params serverapi.ReconcileSandboxParams) (serverapi.ReconcileSandboxRes, error) {
	sandbox, err := h.services.Sandboxes.ReconcileSandbox(ctx, params.ProjectId, params.SandboxId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.SandboxToAPI(sandbox, h.fallbackHarnessConfig(ctx, params.ProjectId))
	if err != nil {
		return nil, err
	}
	return &body, nil
}
