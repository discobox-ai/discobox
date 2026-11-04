package sandboxes

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/proxy/egress"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func (s *Service) CreateAdoptionRequest(ctx context.Context, projectID string, input services.CreateAdoptionRequestBody) (*model.AdoptionRequest, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeSandbox || principal.ProjectID != projectID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "only a sandbox may request adoption")
	}
	if strings.TrimSpace(input.Justification) == "" || len(input.Justification) > 8192 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a justification of at most 8192 bytes is required")
	}
	child, err := s.store.GetSandbox(ctx, projectID, input.ChildSandboxId)
	if err != nil {
		return nil, apperrors.NotFound(err, "child sandbox not found in this project")
	}
	req := &model.AdoptionRequest{ProjectID: projectID, RequesterSandboxID: principal.SandboxID, ChildSandboxID: child.ID, Justification: input.Justification, Status: model.SecretRequestStatusPending}
	if err := s.store.CreateAdoptionRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, s.describeAdoption(ctx, req)
}

func (s *Service) ListAdoptionRequests(ctx context.Context, projectID, status string) ([]model.AdoptionRequest, error) {
	requester := ""
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		requester = principal.SandboxID
	}
	requests, err := s.store.ListAdoptionRequests(ctx, projectID, status, requester)
	if err != nil {
		return nil, err
	}
	for i := range requests {
		if err := s.describeAdoption(ctx, &requests[i]); err != nil {
			return nil, err
		}
	}
	return requests, nil
}

func (s *Service) GetAdoptionRequest(ctx context.Context, projectID, requestID string) (*model.AdoptionRequest, error) {
	req, err := s.store.GetAdoptionRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "adoption request not found")
	}
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox && req.RequesterSandboxID != principal.SandboxID {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "this adoption request belongs to another sandbox")
	}
	return req, s.describeAdoption(ctx, req)
}

func (s *Service) DecideAdoptionRequest(ctx context.Context, projectID, requestID string, approve bool) (*model.AdoptionRequest, error) {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok || principal.Type != auth.PrincipalTypeUser {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "only a person may answer adoption requests")
	}
	result, err := s.store.DecideAdoptionRequest(ctx, projectID, requestID, principal.UserID, approve)
	return result, apperrors.NotFound(err, "adoption request not found")
}

func (s *Service) ReleaseSandbox(ctx context.Context, projectID, sandboxID string) error {
	parentID := ""
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		parentID = principal.SandboxID
	}
	return apperrors.NotFound(s.store.ReleaseSandbox(ctx, projectID, sandboxID, parentID), "sandbox not found")
}

// describeAdoption projects current authority for the person's approval card.
func (s *Service) describeAdoption(ctx context.Context, req *model.AdoptionRequest) error {
	child, err := s.store.GetSandbox(ctx, req.ProjectID, req.ChildSandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	} // The pending request remains deniable after purge.
	req.CurrentParentSandboxID = child.ParentSandboxID
	req.ChildName = services.SandboxDisplayName(child)
	requester, err := s.store.GetSandbox(ctx, req.ProjectID, req.RequesterSandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	req.RequesterName = services.SandboxDisplayName(requester)
	outer, err := s.store.EffectiveSandboxEgress(ctx, requester)
	if err != nil {
		return err
	}
	inner, err := s.store.EffectiveSandboxEgress(ctx, child)
	if err != nil {
		return err
	}
	req.ReadOnly = !egress.Covers(outer, inner)
	return nil
}
