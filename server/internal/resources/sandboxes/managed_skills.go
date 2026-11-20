package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func (s *Service) GetSandboxSkills(ctx context.Context, projectID, sandboxID string) (model.SandboxManagedSkills, error) {
	if err := authorizeSandboxManagement(ctx, s.store, projectID, sandboxID, false); err != nil {
		return model.SandboxManagedSkills{}, err
	}
	current, err := s.store.GetSandboxManagedSkills(ctx, projectID, sandboxID)
	if err != nil {
		return current, err
	}
	box, err := s.store.GetSandbox(ctx, projectID, sandboxID, store.SandboxFromPrimary())
	if err != nil {
		return current, apperrors.NotFound(err, "sandbox not found")
	}
	var status struct {
		Installed *([]string) `json:"installedSkillNames"`
		Revision  *int64      `json:"managedSkillsRevision"`
	}
	_ = json.Unmarshal(box.AgentStatus, &status)
	names := box.SkillNames
	if status.Installed != nil {
		names = *status.Installed
	}
	current.Installed = make(map[string]sandboxconfig.SkillVersion)
	for _, name := range names {
		if sandboxconfig.ValidateSkillName(name) != nil {
			continue
		}
		if version, ok := current.AppliedVersions[name]; ok {
			current.Installed[name] = version
		} else {
			current.Installed[name] = sandboxconfig.SkillVersion{SkillOrigin: sandboxconfig.SkillOrigin{SourceID: "unknown"}}
		}
	}
	switch {
	case current.Desired.Revision > 0 && len(box.AgentStatus) > 0 && status.Revision == nil:
		current.Application = "unsupported"
	case current.Desired.Revision <= current.Applied:
		current.Application = "applied"
	default:
		current.Application = "pending"
	}
	return current, nil
}

func (s *Service) MutateSandboxSkills(ctx context.Context, projectID, sandboxID string, change sandboxconfig.SkillMutation) (model.SandboxManagedSkills, error) {
	result, err := s.store.MutateSandboxSkills(ctx, projectID, sandboxID, auth.ActorFromContext(ctx), change, func(tx *store.Store) error {
		return authorizeSandboxManagement(ctx, tx, projectID, sandboxID, true)
	})
	switch {
	case errors.Is(err, sandboxconfig.ErrSkillRevisionChanged):
		err = apperrors.NewStatusError(http.StatusConflict, err.Error())
	case errors.Is(err, sandboxconfig.ErrInvalidSkillMutation):
		err = apperrors.NewStatusError(http.StatusUnprocessableEntity, err.Error())
	case errors.Is(err, store.ErrNotFound):
		err = apperrors.NotFound(err, "sandbox not found")
	}
	return result, err
}

func authorizeSandboxManagement(ctx context.Context, db *store.Store, projectID, sandboxID string, write bool) error {
	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return apperrors.NewStatusError(http.StatusForbidden, "user or lead access required")
	}
	if principal.Type == auth.PrincipalTypeSandbox && principal.ProjectID == projectID {
		_, err := (auth.SandboxRoleAuthorizer{Store: db}).AuthorizeChild(ctx, principal, sandboxID, write, "manage sandbox runtime intent")
		return err
	}
	if principal.Type != auth.PrincipalTypeUser || principal.UserID == "" {
		return apperrors.NewStatusError(http.StatusForbidden, "user or lead access required")
	}
	box, err := db.GetSandbox(ctx, projectID, sandboxID, store.SandboxFromPrimary())
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	if box.CreatedByUserID != principal.UserID {
		return apperrors.NewStatusError(http.StatusForbidden, "sandbox owner access required")
	}
	return nil
}
