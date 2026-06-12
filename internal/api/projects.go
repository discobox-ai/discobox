package api

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/obot-platform/discobox/internal/model"
)

type ProjectPathInput struct {
	ProjectID string `path:"projectId" doc:"Project ID"`
}

type ListProjectsOutput struct {
	Body ListProjectsBody
}

type ListProjectsBody struct {
	Projects []model.Project `json:"projects" doc:"Projects"`
}

type GetProjectOutput struct {
	Body model.Project
}

// RegisterProjectOperations registers read-only project operations.
func RegisterProjectOperations(api huma.API, service ProjectService) {
	huma.Register(api, huma.Operation{
		OperationID: "list-projects",
		Method:      http.MethodGet,
		Path:        "/projects",
		Summary:     "List projects",
		Tags:        []string{"Projects"},
	}, func(ctx context.Context, input *struct{}) (*ListProjectsOutput, error) {
		projects, err := service.ListProjects(ctx)
		if err != nil {
			return nil, err
		}
		return &ListProjectsOutput{Body: ListProjectsBody{Projects: projects}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-project",
		Method:      http.MethodGet,
		Path:        "/projects/{projectId}",
		Summary:     "Get a project",
		Tags:        []string{"Projects"},
	}, func(ctx context.Context, input *ProjectPathInput) (*GetProjectOutput, error) {
		project, err := service.GetProject(ctx, input.ProjectID)
		if err != nil {
			return nil, err
		}
		return &GetProjectOutput{Body: *project}, nil
	})
}
