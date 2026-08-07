package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListSSHKeys(ctx context.Context, params serverapi.ListSSHKeysParams) (serverapi.ListSSHKeysRes, error) {
	keys, err := h.services.SSHKeys.ListSSHKeys(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListSSHKeysBody](struct {
		SSHKeys any `json:"sshKeys"`
	}{SSHKeys: keys})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateSSHKey(ctx context.Context, req *apimodel.CreateSSHKeyBody, params serverapi.CreateSSHKeyParams) (serverapi.CreateSSHKeyRes, error) {
	key, err := h.services.SSHKeys.CreateSSHKey(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.SSHKey](key)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteSSHKey(ctx context.Context, params serverapi.DeleteSSHKeyParams) (serverapi.DeleteSSHKeyRes, error) {
	if err := h.services.SSHKeys.DeleteSSHKey(ctx, params.ProjectId, params.SshKeyId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteSSHKeyNoContent{}, nil
}
