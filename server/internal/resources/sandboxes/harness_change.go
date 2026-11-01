package sandboxes

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
)

func (s *Service) prepareHarnessChange(ctx context.Context, sb *model.Sandbox, config apimodel.SandboxUpdateConfig) (*model.SandboxHarnessChange, error) {
	selector, hasSelector := config.Harness.Get()
	explicit, hasID := config.HarnessConfigId.Get()
	if !hasSelector && !hasID {
		return nil, nil
	}
	if hasSelector && hasID {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness and harnessConfigId are mutually exclusive")
	}
	if (hasSelector && strings.TrimSpace(selector) == "") || (hasID && strings.TrimSpace(explicit) == "") {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness requires a non-empty slug, name or ID")
	}
	var target *model.HarnessConfig
	var err error
	if hasSelector {
		target, err = s.harnessConfigBySelector(ctx, sb.ProjectID, strings.TrimSpace(selector))
	} else {
		target, err = s.store.GetHarnessConfig(ctx, sb.ProjectID, strings.TrimSpace(explicit))
	}
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	if sb.HarnessConfigID != nil && *sb.HarnessConfigID == target.ID {
		return nil, nil
	}
	if sb.DesiredState != model.DesiredStatePresent || sb.State != model.SandboxStateReady || !sb.Converged() || sb.PendingHarnessChange != nil || sb.HarnessMode == "config" {
		return nil, apperrors.NewStatusError(http.StatusConflict, "harness changes require a settled, stopped sandbox; stop it with `discobox stop DISCOBOX` first")
	}
	if !target.Configured {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("harness %q is not configured; run `discobox admin harness configure %s` first", target.Slug, target.Slug))
	}
	pins, err := createHarnessPins(target)
	if err != nil {
		return nil, err
	}
	candidate := *sb
	services.SandboxPinTarget{Harness: pins}.Apply(&candidate)
	candidate.HarnessConfigID = &target.ID
	candidate.Prompt = nil
	own, err := ownBindingEnvs(ctx, s.store, sb)
	if err != nil {
		return nil, err
	}
	if err := checkRequiredSecrets(ctx, s.store, &candidate, target.ID, own); err != nil {
		return nil, err
	}
	changeID, err := id.New("hchange")
	if err != nil {
		return nil, err
	}
	return &model.SandboxHarnessChange{ID: changeID, Manifest: candidate.SandboxManifest}, nil
}

// A proposal is durable before we ask the pool for a fence. Until that fence
// succeeds it is separate from the effective manifest, so a refused request
// cannot leave behind a spec which later kills a running sandbox.
func (r *SandboxReconciler) applyHarnessChange(ctx context.Context, sb *model.Sandbox) (reconcile.Result, error) {
	proposal := sb.PendingHarnessChange
	if r.engine == nil {
		return reconcile.Result{}, errors.New("reconcile engine is required")
	}
	provider, err := r.resolveProvider(ctx, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	if provider == nil {
		return reconcile.Result{}, errors.New("harness change requires a reachable provider")
	}
	state, err := r.store.OpenSandboxSecretState(ctx, sb)
	if err != nil {
		return reconcile.Result{}, err
	}
	_, _, err = provider.Update(ctx, sandboxRefFromSandbox(sb), state, UpdateOptions{ReserveHarnessChange: proposal.ID})
	if err != nil {
		if !errors.Is(err, sandbox.ErrHarnessChangeRefused) && !errors.Is(err, sandbox.ErrNotFound) && !errors.Is(err, sandbox.ErrNoContainer) && !errors.Is(err, sandbox.ErrArchived) && !errors.Is(err, sandbox.ErrPoolAgentUnsupported) {
			return reconcile.Result{}, err
		}
		sb.PendingHarnessChange = nil
		sb.PendingHarnessConfigID = nil
		sb.HarnessChangeError = "harness change refused: " + err.Error()
		if errors.Is(err, sandbox.ErrHarnessChangeRefused) {
			sb.HarnessChangeError = fmt.Sprintf("stop the sandbox with `discobox stop %s` before changing its harness", sb.ID)
		}
		sb.ObservedGeneration = sb.Generation
		return reconcile.Result{}, r.update(ctx, sb, sb.Generation)
	}
	err = r.store.Transaction(ctx, func(st *store.Store, db *gorm.DB) error {
		current, err := st.GetSandbox(ctx, sb.ProjectID, sb.ID)
		if err != nil {
			return err
		}
		if current.Generation != sb.Generation || current.PendingHarnessChange == nil || current.PendingHarnessChange.ID != proposal.ID {
			return reconcile.ErrSuperseded
		}
		current.IncrementGeneration()
		current.SandboxManifest = proposal.Manifest
		current.HarnessChangeID = proposal.ID
		current.PendingHarnessChange = nil
		current.PendingHarnessConfigID = nil
		current.HarnessChangeError = ""
		// The new harness config's bindings replace the old one's; the
		// sandbox's own stay, and win for their variables.
		if _, err := materializeSandboxSecrets(ctx, st, current); err != nil {
			return err
		}
		if err := st.UpdateSandbox(ctx, current, store.WithGeneration(sb.Generation)); err != nil {
			return err
		}
		if err := r.engine.MarkDirtyTx(ctx, db, SandboxResourceType, SandboxDirtyID(sb.ProjectID, sb.ID)); err != nil {
			return err
		}
		*sb = *current
		return nil
	})
	if err != nil {
		return reconcile.Result{}, err
	}
	return r.ensure(ctx, sb, sb.Generation)
}

// ownBindingEnvs are the variables a sandbox's own use bindings deliver.
func ownBindingEnvs(ctx context.Context, st *store.Store, sb *model.Sandbox) (map[string]struct{}, error) {
	bindings, err := st.ListSecretBindings(ctx, sb.ProjectID, store.SecretBindingFilter{TargetKind: model.SecretBindingTargetSandbox, TargetID: sb.ID})
	if err != nil {
		return nil, err
	}
	own := map[string]struct{}{}
	for _, binding := range bindings {
		if binding.Use {
			own[binding.EnvName] = struct{}{}
		}
	}
	return own, nil
}
