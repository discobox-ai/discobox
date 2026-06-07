package service

import "context"

func (s *Service) CreateSandboxAuthToken(ctx context.Context, projectID, sandboxID string) (string, error) {
	if s == nil || s.sandboxAuth == nil {
		return "", nil
	}
	sb, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return "", apiError(err, "sandbox not found")
	}
	return s.sandboxAuth.CreateToken(ctx, sb.ProjectID, sb.CreatedByUserID)
}
