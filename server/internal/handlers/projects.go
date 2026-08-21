package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
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

func (h *Handler) CreateProject(ctx context.Context, req *apimodel.CreateProjectBody) (serverapi.CreateProjectRes, error) {
	project, err := h.services.Projects.CreateProject(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
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

func (h *Handler) UpdateProject(ctx context.Context, req *apimodel.UpdateProjectBody, params serverapi.UpdateProjectParams) (serverapi.UpdateProjectRes, error) {
	project, err := h.services.Projects.UpdateProject(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteProject(ctx context.Context, params serverapi.DeleteProjectParams) (serverapi.DeleteProjectRes, error) {
	if err := h.services.Projects.DeleteProject(ctx, params.ProjectId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteProjectNoContent{}, nil
}

func (h *Handler) SetDefaultProject(ctx context.Context, params serverapi.SetDefaultProjectParams) (serverapi.SetDefaultProjectRes, error) {
	project, err := h.services.Projects.SetDefaultProject(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Project](project)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
