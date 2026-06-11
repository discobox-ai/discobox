package sandboxservice

import (
	"context"

	"github.com/obot-platform/disco2/internal/sandboxauth"
)

func (s *Service) CreateSandboxAuthToken(ctx context.Context, projectID, sandboxID string) (string, error) {
	if s == nil || s.sandboxAuth == nil {
		return "", nil
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return "", mapAPIError(err, "sandbox not found")
	}
	project, err := s.store.GetProject(ctx, sb.ProjectID)
	if err != nil {
		return "", mapAPIError(err, "project not found")
	}
	return s.sandboxAuth.CreateToken(ctx, sandboxauth.TokenClaims{
		TenantID:  project.TenantID,
		ProjectID: sb.ProjectID,
		SandboxID: sb.ID,
		UserID:    sb.CreatedByUserID,
	})
}
