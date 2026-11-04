package harnessconfigs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The secrets saved harness changes create (ConfiguredSecretIDs): how one is
// named, routed and bound to its config.

// bindHarnessSecret makes envName on the harness config deliver secretID for
// use: the one binding save-back writes (ADR 26-10-27-880 §5),
// with no routes of its own — it inherits the secret's — and no expiry. One
// already standing for another secret is replaced. The sandboxes running the
// config are marked.
func (s *Service) bindHarnessSecret(ctx context.Context, txStore *store.Store, txDB *gorm.DB, config *model.HarnessConfig, envName, secretID string) error {
	actor := auth.ActorFromContext(ctx)
	binding := &model.SecretBinding{
		ProjectID:  config.ProjectID,
		SecretID:   secretID,
		TargetKind: model.SecretBindingTargetHarnessConfig,
		TargetID:   config.ID,
		Use:        true,
		EnvName:    envName,
		CreatedBy:  actor,
	}
	removed, err := txStore.PutTargetUseBinding(ctx, binding, store.BindingChange{Actor: actor})
	if err != nil {
		return err
	}
	if s.sandboxes == nil {
		return nil
	}
	return s.sandboxes.MarkBindingHoldersDirtyTx(ctx, txStore, txDB, config.ProjectID, append(removed, *binding))
}

// configuredRoutes are where a secret a save-back stores may go: the routes
// the image declares for its variable, else the host its provider names —
// that exact host — else anywhere (ADR 26-10-27-939 §2).
func configuredRoutes(config *model.HarnessConfig, envName, host string) []model.SecretRoute {
	if declared := declaredRoutesByEnv(config)[envName]; len(declared) > 0 {
		return declared
	}
	if host != "" {
		return []model.SecretRoute{{Hosts: []string{host}}}
	}
	return []model.SecretRoute{{Hosts: []string{secretroute.AnyHost}}}
}

// secretNameTaken reports whether another secret of the project has name.
func (s *Service) secretNameTaken(ctx context.Context, projectID, name, exceptID string) (bool, error) {
	secret, err := s.store.GetSecretByName(ctx, projectID, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return secret.ID != exceptID, nil
}

// freeSecretName is name when no secret of the project has it, else the
// harness config's slug before it, else that with the first free number
// after it: a secret's name is unique in its project, and two harnesses may
// each store a credential under the same variable.
func (s *Service) freeSecretName(ctx context.Context, config *model.HarnessConfig, name string) (string, error) {
	candidates := []string{name, config.Slug + "-" + name}
	for n := 2; n < 100; n++ {
		candidates = append(candidates, fmt.Sprintf("%s-%s-%d", config.Slug, name, n))
	}
	for _, candidate := range candidates {
		taken, err := s.secretNameTaken(ctx, config.ProjectID, candidate, "")
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no free name for secret %q", name)
}

// syncConfiguredSecretRoutes writes the routes the config's image declares for
// each secret onto the secret a save-back created for it (ADR 26-10-02-924),
// so every request the harness sends it in must fall on one. Only those
// secrets are written: one a person bound to the config is theirs to describe.
// It runs in the caller's transaction, wherever the image is re-read, and marks the sandboxes holding
// a secret whose routes changed for reconcile in it: the routes and their
// delivery commit together or not at all, so a retry finds them still to do.
func (s *Service) syncConfiguredSecretRoutes(ctx context.Context, txStore *store.Store, txDB *gorm.DB, config *model.HarnessConfig) error {
	configured := make(map[string]bool, len(config.ConfiguredSecretIDs))
	for _, secretID := range config.ConfiguredSecretIDs {
		configured[strings.TrimSpace(secretID)] = true
	}
	if len(configured) == 0 {
		return nil
	}
	declared := declaredRoutesByEnv(config)
	bindings, err := txStore.ListTargetUseBindings(ctx, config.ProjectID, model.SecretBindingTargetHarnessConfig, config.ID)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		// An image that declares no routes for a variable says nothing about
		// where its secret goes, and leaves it as the save wrote it.
		if !configured[binding.SecretID] || len(declared[binding.EnvName]) == 0 {
			continue
		}
		secret, err := txStore.GetSecret(ctx, config.ProjectID, binding.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return err
		}
		changed, err := secret.ReplaceRoutes(declared[binding.EnvName])
		if err != nil {
			return fmt.Errorf("routes of configured secret %q: %w", binding.EnvName, err)
		}
		if !changed {
			continue
		}
		if err := txStore.UpdateSecret(ctx, secret); err != nil {
			return fmt.Errorf("write routes of configured secret %q: %w", binding.EnvName, err)
		}
		if s.sandboxes != nil {
			if err := s.sandboxes.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, config.ProjectID, secret.ID); err != nil {
				return fmt.Errorf("mark the holders of configured secret %q: %w", binding.EnvName, err)
			}
		}
	}
	return nil
}

// declaredRoutesByEnv is the routes the config's image declares, by the
// environment variable each secret is bound to.
func declaredRoutesByEnv(config *model.HarnessConfig) map[string][]model.SecretRoute {
	declared := make(map[string][]model.SecretRoute, len(config.Secrets))
	for _, secret := range config.Secrets {
		declared[secret.Name] = secret.Routes
	}
	return declared
}
