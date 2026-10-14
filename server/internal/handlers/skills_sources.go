package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) ListProjectSkillsSources(ctx context.Context, params serverapi.ListProjectSkillsSourcesParams) (serverapi.ListProjectSkillsSourcesRes, error) {
	sources, err := h.services.SkillsSources.ListProjectSkillsSources(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListProjectSkillsSourcesBody](struct {
		SkillsSources any `json:"skillsSources"`
	}{SkillsSources: sources})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreateProjectSkillsSource(ctx context.Context, req *apimodel.CreateProjectSkillsSourceBody, params serverapi.CreateProjectSkillsSourceParams) (serverapi.CreateProjectSkillsSourceRes, error) {
	source, err := h.services.SkillsSources.CreateProjectSkillsSource(ctx, params.ProjectId, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ProjectSkillsSource](source)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeleteProjectSkillsSource(ctx context.Context, params serverapi.DeleteProjectSkillsSourceParams) (serverapi.DeleteProjectSkillsSourceRes, error) {
	if err := h.services.SkillsSources.DeleteProjectSkillsSource(ctx, params.ProjectId, params.SkillsSourceId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteProjectSkillsSourceNoContent{}, nil
}
