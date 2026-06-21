package handlers

import (
	"context"

	serverapi "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	services "github.com/obot-platform/discobox/server/internal/services"
)

func (h *Handler) ListProjects(ctx context.Context) (serverapi.ListProjectsRes, error) {
	projects, err := h.services.Projects.ListProjects(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListProjectsBody](struct {
		Projects any `json:"projects"`
	}{Projects: projects})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetProject(ctx context.Context, params serverapi.GetProjectParams) (serverapi.GetProjectRes, error) {
	project, err := h.services.Projects.GetProject(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
