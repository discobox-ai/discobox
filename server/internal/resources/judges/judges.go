// Package judges keeps a project's judge converged: the discobox that answers
// judging asks and nothing else (ADR 0150 §1).
//
// A judge is Discobox's own. It is not the project's work, it is not listed
// with it, and it exists exactly when the project can have one: a pool to run
// it in, recorded when the project's first pool was made, and a default harness
// that is configured, which is where its image and its model credential come
// from. Change the default harness and the judge is replaced with it; take it
// away and the judge goes.
package judges

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/judge"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// JudgeResourceType is the reconciler's resource type; its id is a project ID,
// because a project has one judge.
const JudgeResourceType = "judge"

// Sandboxes is what this package needs of the sandbox service: a judge is an
// ordinary discobox in every way except what it is for, so it is created,
// found and deleted through the same machinery as any other.
type Sandboxes interface {
	CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error)
	// PurgeSandbox rather than DeleteSandbox: deleting archives, and an
	// archived discobox keeps the pool it ran in, which would leave a judge
	// nobody can see holding a pool open forever. A judge holds none of the
	// project's work, so there is nothing an archive would be keeping.
	PurgeSandbox(ctx context.Context, projectID, sandboxID string) error
}

// Service converges the judges of every project.
type Service struct {
	store     *store.Store
	sandboxes Sandboxes
	leases    Leases
	uses      Uses
	logger    *slog.Logger
	// enabled is whether this server judges at all. It is a server's decision
	// rather than a project's: judging puts a model in front of every
	// credential-bearing request, and a server that has not asked for that
	// keeps resolving credentials the way it always did.
	enabled bool
}

func New(appStore *store.Store, sandboxes Sandboxes, logger *slog.Logger, enabled bool) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{store: appStore, sandboxes: sandboxes, logger: logger, enabled: enabled}
}

// Reconcile brings one project's judge to what the project says it should be.
func (s *Service) Reconcile(ctx context.Context, projectID string) (reconcile.Result, error) {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	existing, err := s.judge(ctx, project)
	if err != nil {
		return reconcile.Result{}, err
	}

	wanted, why, err := s.wanted(ctx, project, true)
	if err != nil {
		return reconcile.Result{}, err
	}
	if wanted == nil {
		// Nothing to run a judge from. An existing one is taken away rather
		// than left behind answering from a harness the project no longer
		// uses; its absence is what makes a use-scoped credential refuse, and
		// that is the ADR's answer, not a stale judge.
		if existing != nil {
			s.logger.InfoContext(ctx, "removing the project's judge", "projectId", projectID, "sandboxId", existing.ID, "reason", why)
			if err := s.sandboxes.PurgeSandbox(ctx, projectID, existing.ID); err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, s.remember(ctx, projectID, "")
	}

	if existing != nil {
		if matches(existing, wanted) {
			return reconcile.Result{}, nil
		}
		// A judge is replaced rather than edited: what changed is which
		// harness it runs, which is its image, its settings and the credential
		// it answers with. It holds no work, so there is nothing to carry over.
		s.logger.InfoContext(ctx, "replacing the project's judge", "projectId", projectID, "sandboxId", existing.ID,
			"harnessConfigId", wanted.harnessConfigID, "image", wanted.imageDigest)
		if err := s.sandboxes.PurgeSandbox(ctx, projectID, existing.ID); err != nil {
			return reconcile.Result{}, err
		}
	}

	created, err := s.create(ctx, projectID, wanted)
	if err != nil {
		return reconcile.Result{}, err
	}
	if err := s.remember(ctx, projectID, created.ID); err != nil {
		// Nothing points at it, so nothing would ever adopt it, list it, or
		// take it away: the next run makes another and this one runs on
		// forever. Undo the create instead and let the retry do it again.
		if deleteErr := s.sandboxes.PurgeSandbox(ctx, projectID, created.ID); deleteErr != nil {
			s.logger.ErrorContext(ctx, "a judge was created that nothing points at, and could not be removed",
				"projectId", projectID, "sandboxId", created.ID, "error", deleteErr)
		}
		return reconcile.Result{}, err
	}
	s.logger.InfoContext(ctx, "the project has a judge", "projectId", projectID, "sandboxId", created.ID,
		"poolId", created.PoolID, "harnessConfigId", wanted.harnessConfigID)
	return reconcile.Result{}, nil
}

// ScanDirty names every project, so a judge converges even when whatever
// changed did not think to say so. It is the reconcile engine's Scanner, which
// is a type assertion on the registered reconciler: a signature that does not
// match it compiles, registers, and never runs.
func (s *Service) ScanDirty(ctx context.Context) ([]string, error) {
	projects, err := s.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(projects))
	for _, project := range projects {
		ids = append(ids, project.ID)
	}
	return ids, nil
}

// The engine holds this package to its Scanner, so a signature that drifts is
// a compile error rather than a scan that silently never runs.
var _ reconcile.Scanner = (*Service)(nil)

// judge is the discobox this project's judge is, or nil. It is the one the
// project points at: judge mode is something anybody may create, so the mode
// alone does not say which one Discobox runs. A pointer at a discobox that is
// no longer there is the same as none.
func (s *Service) judge(ctx context.Context, project *model.Project) (*model.Sandbox, error) {
	sandboxID := strings.TrimSpace(project.JudgeSandboxID)
	if sandboxID == "" {
		return nil, nil
	}
	sandbox, err := s.store.GetSandbox(ctx, project.ID, sandboxID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return sandbox, nil
}

// remember records which discobox is the project's judge, or that it has none.
func (s *Service) remember(ctx context.Context, projectID, sandboxID string) error {
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if project.JudgeSandboxID == sandboxID {
		return nil
	}
	project.JudgeSandboxID = sandboxID
	return s.store.UpsertProject(ctx, project)
}

// judgeSpec is the judge a project should have.
type judgeSpec struct {
	poolID          string
	harnessConfigID string
	imageDigest     string
}

// wanted is the judge this project should have, or nil and why it has none. An
// error is neither: a store that cannot be read says nothing about what the
// project wants, and treating it as "gone" would take a working judge away on
// a moment's database trouble.
func (s *Service) wanted(ctx context.Context, project *model.Project, record bool) (*judgeSpec, string, error) {
	// Off is off everywhere, and it reads the same as any other reason a
	// project has no judge: nothing is created, and a judge created while it
	// was on is taken away by the convergence that finds none wanted.
	if !s.enabled {
		return nil, "this server does not judge credential use", nil
	}
	poolID, err := s.judgePool(ctx, project, record)
	if err != nil {
		return nil, "", err
	}
	if poolID == "" {
		return nil, "the project has no pool to run a judge in", nil
	}
	harnessConfigID := strings.TrimSpace(project.DefaultHarnessConfigID)
	if harnessConfigID == "" {
		return nil, "the project has no default harness", nil
	}
	config, err := s.store.GetHarnessConfig(ctx, project.ID, harnessConfigID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			return nil, "", err
		}
		return nil, "the project's default harness is gone", nil
	}
	if !config.Configured {
		return nil, "the project's default harness is not configured", nil
	}
	// A harness with no prompting wrapper cannot judge. Nothing an image
	// declares says so today, so the one that is known not to is named here
	// and anything else is tried; a harness that turns out not to answer
	// refuses its asks, which is the same answer arrived at later.
	if config.Slug == shellSlug {
		return nil, "the project's default harness runs no model", nil
	}
	return &judgeSpec{poolID: poolID, harnessConfigID: config.ID, imageDigest: config.ImageDigest}, "", nil
}

// judgePool is where this project's judge runs, choosing one and recording it
// if the project has none yet.
//
// It is recorded rather than resolved afresh each time, because a judge is
// where it is: its pool is fixed at create and a project that answered
// differently later would keep replacing it. But it cannot only ever be
// written at bootstrap — a project made through the API never went through
// one, and a database that predates this field would have none forever, which
// is a project whose credentials can never be judged.
func (s *Service) judgePool(ctx context.Context, project *model.Project, record bool) (string, error) {
	if recorded := strings.TrimSpace(project.JudgePoolID); recorded != "" {
		pool, err := s.store.GetPool(ctx, project.ID, recorded)
		switch {
		case err == nil && pool.DesiredState != model.DesiredStateDeleted:
			return recorded, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return "", err
		}
		// Gone, or on its way out. A judge that stayed in a pool being deleted
		// would hold that pool open: the delete gate lets it through because a
		// judge is not the project's work, and the pool's own convergence then
		// counts it and refuses forever. So choose again.
	}
	pools, err := s.store.ListPools(ctx, project.ID)
	if err != nil {
		return "", err
	}
	chosen := ""
	for _, pool := range pools {
		if pool.DesiredState == model.DesiredStateDeleted {
			continue
		}
		// The project's default is the one the environment made, and on every
		// platform Discobox supports that is a pool of Linux containers, which
		// is what a judge is.
		if pool.ID == project.DefaultPoolID {
			chosen = pool.ID
			break
		}
		if chosen == "" {
			chosen = pool.ID
		}
	}
	if chosen == "" || !record || chosen == project.JudgePoolID {
		return chosen, nil
	}
	// Written by the convergence alone. Reading which judge a project has is
	// also what a pool asking for a verdict does, and a read that writes is a
	// write from every pool at once.
	current, err := s.store.GetProject(ctx, project.ID)
	if err != nil {
		return "", err
	}
	current.JudgePoolID = chosen
	if err := s.store.UpsertProject(ctx, current); err != nil {
		return "", err
	}
	return chosen, nil
}

// shellSlug is the built-in harness that installs no agent.
const shellSlug = "shell"

// matches reports whether a judge is the one the project should have, and is
// in a state to answer as one.
func matches(existing *model.Sandbox, wanted *judgeSpec) bool {
	// A judge that failed to come up is not a judge. Left alone it would be a
	// project that has one by every other measure and can never get a verdict
	// out of it; replacing it is what a level-triggered convergence is for,
	// and it costs one create per scan rather than a wedge nobody notices.
	switch {
	case existing.State == model.SandboxStateFailed:
		return false
	// DeleteSandbox archives rather than destroys (ADR 0022), so this is what
	// a judge that has been taken away looks like — by this package, or by a
	// person reaching for its ID. Either way it answers nothing.
	case existing.DesiredState == model.DesiredStateDeleted,
		existing.DesiredState == model.DesiredStateArchived:
		return false
	}
	if existing.PoolID != wanted.poolID {
		return false
	}
	if existing.HarnessConfigID == nil || *existing.HarnessConfigID != wanted.harnessConfigID {
		return false
	}
	// The image the harness is pinned to is part of what the judge is: a
	// re-pinned harness is a different judge, and the ADR says the runtime is
	// replaced when the configuration behind it changes.
	return existing.ImageDigest == wanted.imageDigest
}

func (s *Service) create(ctx context.Context, projectID string, wanted *judgeSpec) (*model.Sandbox, error) {
	name, err := judgeName()
	if err != nil {
		return nil, err
	}
	var input services.CreateSandboxBody
	input.Config.Name = name
	input.SetPoolId(serverapi.NewOptString(wanted.poolID))
	input.Config.SetHarnessConfigId(serverapi.NewOptString(wanted.harnessConfigID))
	// The image, the settings and the credential all follow the harness
	// config, exactly as they do for a discobox somebody made.
	input.Config.SetHarnessMode(serverapi.NewOptSandboxCreateConfigHarnessMode(serverapi.SandboxCreateConfigHarnessModeJudge))
	return s.sandboxes.CreateSandbox(ctx, projectID, input)
}

// judgeName is what the judge is called. A name is unique within a project and
// is an addressable handle, so it cannot be the fixed word "judge": that is a
// name somebody's own discobox may already have.
func judgeName() (string, error) {
	unique, err := id.New(id.PrefixSandbox)
	if err != nil {
		return "", err
	}
	// The ID's own suffix, without its prefix or the separator after it: a
	// name is what a person reads in a listing, and "judge-_7226f23" reads as
	// a bug.
	trimmed := strings.TrimPrefix(unique, id.PrefixSandbox)
	trimmed = strings.TrimLeft(trimmed, "_-")
	if len(trimmed) > 8 {
		trimmed = trimmed[:8]
	}
	return fmt.Sprintf("%s-%s", judge.Role, trimmed), nil
}
