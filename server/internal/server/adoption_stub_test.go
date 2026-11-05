package server

import (
	"context"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (*routerTestServices) CreateAdoptionRequest(context.Context, string, services.CreateAdoptionRequestBody) (*model.AdoptionRequest, error) {
	return nil, nil
}
func (*routerTestServices) ListAdoptionRequests(context.Context, string, string) ([]model.AdoptionRequest, error) {
	return []model.AdoptionRequest{}, nil
}
func (*routerTestServices) GetAdoptionRequest(context.Context, string, string) (*model.AdoptionRequest, error) {
	return nil, nil
}
func (*routerTestServices) DecideAdoptionRequest(context.Context, string, string, bool) (*model.AdoptionRequest, error) {
	return nil, nil
}
func (*routerTestServices) ReleaseSandbox(context.Context, string, string) error { return nil }

func (*routerTestServices) CreateAPIAccessRequest(context.Context, string, services.CreateAPIAccessRequestBody) (*model.APIAccessRequest, error) {
	return nil, nil
}
func (*routerTestServices) ListAPIAccessRequests(context.Context, string, string) ([]model.APIAccessRequest, error) {
	return []model.APIAccessRequest{}, nil
}
func (*routerTestServices) GetAPIAccessRequest(context.Context, string, string) (*model.APIAccessRequest, error) {
	return nil, nil
}
func (*routerTestServices) DecideAPIAccessRequest(context.Context, string, string, bool) (*model.APIAccessRequest, error) {
	return nil, nil
}
