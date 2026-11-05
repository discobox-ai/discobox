package sandboxes

import (
	"context"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
)

func (s *Service) CreateAPIAccessRequest(ctx context.Context, projectID string, input services.CreateAPIAccessRequestBody) (*model.APIAccessRequest, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeSandbox || principal.ProjectID != projectID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "only a sandbox may request API access")
	}
	if strings.TrimSpace(input.Justification) == "" || len(input.Justification) > 8192 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a justification of at most 8192 bytes is required")
	}
	req := &model.APIAccessRequest{ProjectID: projectID, RequesterSandboxID: principal.SandboxID, Justification: input.Justification, Status: model.SecretRequestStatusPending}
	if err := s.store.CreateAPIAccessRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}
func (s *Service) ListAPIAccessRequests(ctx context.Context, projectID, status string) ([]model.APIAccessRequest, error) {
	requester := ""
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Type == auth.PrincipalTypeSandbox {
		requester = p.SandboxID
	}
	return s.store.ListAPIAccessRequests(ctx, projectID, status, requester)
}
func (s *Service) GetAPIAccessRequest(ctx context.Context, projectID, requestID string) (*model.APIAccessRequest, error) {
	req, err := s.store.GetAPIAccessRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "API access request not found")
	}
	if p, ok := auth.PrincipalFromContext(ctx); ok && p.Type == auth.PrincipalTypeSandbox && req.RequesterSandboxID != p.SandboxID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "this API access request belongs to another sandbox")
	}
	return req, nil
}
func (s *Service) DecideAPIAccessRequest(ctx context.Context, projectID, requestID string, approve bool) (*model.APIAccessRequest, error) {
	p, ok := auth.PrincipalFromContext(ctx)
	if !ok || p.Type != auth.PrincipalTypeUser {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "only a person may answer API access requests")
	}
	req, err := s.store.DecideAPIAccessRequest(ctx, projectID, requestID, p.UserID, approve)
	return req, apperrors.NotFound(err, "API access request not found")
}
