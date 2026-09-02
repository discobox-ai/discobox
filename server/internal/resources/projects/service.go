// Package projects owns the Project resource: the ownership and membership
// boundary every other resource is scoped to. A project is also the unit a
// user switches between, so exactly one of a user's projects carries the
// default flag that `/projects/default` resolves.
package projects

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// ProviderInstances, Pools, and HarnessConfigs are the writes project creation
// delegates when it copies configuration. Copying goes through the owning
// service rather than the store so each resource's create-time behavior still
// runs: provider config validation and instance resolution, pool reconcile
// scheduling, and built-in harness seeding against current images.
type ProviderInstances interface {
	CreateSandboxProviderInstance(ctx context.Context, projectID string, input services.CreateSandboxProviderInstanceBody) (*model.SandboxProviderInstance, error)
}

type Pools interface {
	CreatePool(ctx context.Context, projectID string, input services.CreatePoolBody) (*model.Pool, error)
}

type HarnessConfigs interface {
	SeedBuiltIns(ctx context.Context, projectID string) error
}

type Service struct {
	store     *store.Store
	providers ProviderInstances
	pools     Pools
	harnesses HarnessConfigs
}

func NewService(store *store.Store, providers ProviderInstances, pools Pools, harnesses HarnessConfigs) *Service {
	return &Service{store: store, providers: providers, pools: pools, harnesses: harnesses}
}

func (s *Service) ListProjects(ctx context.Context) ([]model.Project, error) {
	if userID, err := auth.UserID(ctx); err == nil {
		return s.store.ListProjectsForUser(ctx, userID)
	}
	return s.store.ListProjects(ctx)
}

func (s *Service) GetProject(ctx context.Context, projectID string) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	return project, nil
}

// CreateProject creates a project owned by the calling user and seeds it. The
// built-in harnesses are always seeded, exactly as they are for the default
// project, so a new project starts with the same harness catalog. Everything
// else is opt-in through the copy inputs; see copy.go.
func (s *Service) CreateProject(ctx context.Context, input services.CreateProjectBody) (*model.Project, error) {
	userID, err := auth.UserID(ctx)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "creating a project requires a user")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "project name is required")
	}
	if err := s.requireFreeName(ctx, userID, name); err != nil {
		return nil, err
	}
	plan, err := s.resolveCopyPlan(ctx, userID, input)
	if err != nil {
		return nil, err
	}

	project := &model.Project{
		ID:          id.NewString(id.PrefixProject),
		OwnerUserID: userID,
		Name:        name,
	}
	if err := s.store.CreateProject(ctx, project); err != nil {
		return nil, err
	}
	if _, err := s.store.CreateProjectMemberIfNotExists(ctx, &model.ProjectMember{
		ProjectID: project.ID,
		UserID:    userID,
		Role:      "owner",
	}); err != nil {
		return nil, s.abandon(ctx, project.ID, err)
	}
	if err := s.harnesses.SeedBuiltIns(ctx, project.ID); err != nil {
		return nil, s.abandon(ctx, project.ID, err)
	}
	if err := s.copyInto(ctx, project, plan); err != nil {
		return nil, err
	}
	return s.GetProject(ctx, project.ID)
}

// UpdateProject edits a project's own settings — its name, how long its
// archived sandboxes are kept, whether its stopped sandboxes follow their
// harness image, and whether it has shown its introduction.
// Membership and ownership are not editable here; they are their own resource.
func (s *Service) UpdateProject(ctx context.Context, projectID string, input services.UpdateProjectBody) (*model.Project, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	if name, ok := input.Name.Get(); ok {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "project name is required")
		}
		if name != project.Name {
			if err := s.requireFreeName(ctx, project.OwnerUserID, name); err != nil {
				return nil, err
			}
			project.Name = name
		}
	}
	if retention, ok := input.ArchiveRetentionSeconds.Get(); ok {
		if retention < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "archive retention cannot be negative")
		}
		// Zero is meaningful rather than absent: it restores the server default,
		// which the project then tracks as it changes (ADR 0022 §4).
		project.ArchiveRetentionSeconds = retention
	}
	if policy, ok := input.SandboxUpgradePolicy.Get(); ok {
		// Empty is meaningful rather than absent, exactly as zero is for
		// retention above: it restores the server default, which the project
		// then tracks as it changes (ADR 0082 §3).
		value := strings.TrimSpace(string(policy))
		if value != "" && value != model.SandboxUpgradePolicyAutomatic && value != model.SandboxUpgradePolicyManual {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"sandbox upgrade policy must be \"automatic\" or \"manual\"")
		}
		project.SandboxUpgradePolicy = value
	}
	if welcomed, ok := input.Welcomed.Get(); ok {
		// Settable both ways. Setting it is the launcher saying it has done the
		// welcoming; clearing it is how someone asks to be shown it again,
		// which is worth having for a screen that otherwise appears once.
		project.Welcomed = welcomed
	}
	if err := s.store.UpsertProject(ctx, project); err != nil {
		return nil, err
	}
	return s.GetProject(ctx, projectID)
}

// DeleteProject removes an empty project. Sandboxes and pools own runtime that
// has to be torn down through their own reconcilers, so a project holding
// either is refused rather than cascaded; the default project is refused too,
// since `-p default` and every unqualified request resolve through it.
func (s *Service) DeleteProject(ctx context.Context, projectID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return apiError(err, "project not found")
	}
	if project.Default {
		return apperrors.NewStatusError(http.StatusConflict, "project is the default project; make another project the default before deleting it")
	}
	sandboxes, err := s.store.CountSandboxesForProject(ctx, projectID)
	if err != nil {
		return err
	}
	if sandboxes > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "project has sandboxes")
	}
	pools, err := s.store.CountPoolsForProject(ctx, projectID)
	if err != nil {
		return err
	}
	if pools > 0 {
		return apperrors.NewStatusError(http.StatusConflict, "project has pools")
	}
	return apiError(s.store.DeleteProject(ctx, projectID), "project not found")
}

// SetDefaultProject moves the calling user's default-project flag. There is no
// unset: the flag is what `/projects/default` resolves, so a user always has
// exactly one default.
func (s *Service) SetDefaultProject(ctx context.Context, projectID string) (*model.Project, error) {
	userID, err := auth.UserID(ctx)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "setting the default project requires a user")
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	project, err := s.store.SetDefaultProjectForUser(ctx, userID, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	return project, nil
}

// requireFreeName keeps a user's project names unique. Name is the only handle
// a project has besides its ID, and the CLI resolves it, so two projects
// sharing one would make every name-based selection ambiguous.
func (s *Service) requireFreeName(ctx context.Context, ownerUserID, name string) error {
	_, err := s.store.GetProjectByOwnerAndName(ctx, ownerUserID, name)
	switch {
	case err == nil:
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("project %q already exists", name))
	case errors.Is(err, store.ErrNotFound):
		return nil
	default:
		return err
	}
}

// abandon removes a project whose seeding failed before any runtime was
// created, so a failed create does not leave a half-built project behind. The
// original error is what the caller sees.
func (s *Service) abandon(ctx context.Context, projectID string, cause error) error {
	if err := s.store.DeleteProject(ctx, projectID); err != nil {
		return errors.Join(cause, fmt.Errorf("roll back project %s: %w", projectID, err))
	}
	return cause
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}
