package harnessconfigs

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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

// syncConfiguredSecretRoutes adds the image's static/provider policy as an
// independent restriction on each owned secret (ADR 26-11-04-772),
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
		// A manually bound secret is a person's policy, never the bundle's to rewrite.
		if !configured[binding.SecretID] {
			continue
		}
		// Locked: the routes read are written back with the constraint, and
		// judge which bindings it strands, so a person's edit committed in
		// between would be written over and misjudged.
		secret, err := txStore.LockSecret(ctx, config.ProjectID, binding.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return err
		}
		routes, found := declared[binding.EnvName]
		if config.SaveBack != nil {
			for _, provider := range config.SaveBack.Providers {
				if _, matches := config.SaveBack.Provider(provider.ID, binding.EnvName); matches {
					if provider.ManualRoutes != "" || len(provider.CredentialRoutes(secret.Type)) > 0 {
						routes = nil
						for _, route := range provider.CredentialRoutes(secret.Type) {
							routes = append(routes, model.SecretRoute(route))
						}
						found = true
					}
					break
				}
			}
		}
		if !found {
			continue
		}
		prior := *secret
		changed, err := secret.RestrictRoutes(routes)
		if err != nil {
			return fmt.Errorf("routes of configured secret %q: %w", binding.EnvName, err)
		}
		if !changed {
			continue
		}
		if err := txStore.UpdateSecret(ctx, secret); err != nil {
			return fmt.Errorf("write routes of configured secret %q: %w", binding.EnvName, err)
		}
		if err := s.removeStrandedBindingsTx(ctx, txStore, txDB, &prior, secret, "the routes harness config "+config.ID+" declares"); err != nil {
			return fmt.Errorf("remove the bindings configured secret %q's routes strand: %w", binding.EnvName, err)
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

// removeStrandedBindingsTx takes from the standing bindings of secret the
// routes that the route constraint a harness writer just added to it, from
// prior's, strands (store.StrandedBodyPlaceBindings, #1105): a body place no
// list allows any more, which the proxy would refuse on every use while the
// binding stood reported as granted. A config apply and a login save-back run
// with nobody to read a refusal, and refusing would fail the apply or lose the
// login, so the stranded routes go instead: a binding keeps the routes that
// still work, and one left with no open route is removed, each change
// recorded with what caused it, as taking raw away removes raw bindings. The
// discobox that used a removed route asks again, and a person's new grant is
// checked against the routes as they now are. Each binding is read locked and
// changed as it now stands, so a person's edit to it is kept. Their holders
// are marked in the same transaction.
func (s *Service) removeStrandedBindingsTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, prior, secret *model.Secret, by string) error {
	stranded, err := txStore.StrandedBodyPlaceBindings(ctx, prior, secret, time.Now().UTC())
	if err != nil {
		return err
	}
	for _, sb := range stranded {
		change := store.BindingChange{
			Actor: auth.ActorFromContext(ctx),
			Cause: fmt.Sprintf("%s strand a route of it on secret %s: %v", by, secret.ID, sb.Reason),
		}
		// The binding as it stands now, locked: a person's edit to it since
		// it was listed is kept, and only the stranded routes still on it go.
		current, err := txStore.LockSecretBinding(ctx, secret.ProjectID, sb.Binding.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		kept, took := withoutRoutes(current.Routes, sb.Stranded)
		if !took {
			continue
		}
		var changed []model.SecretBinding
		if slices.ContainsFunc(kept, func(r model.SecretRoute) bool { return !r.Blocked }) {
			current.Routes = kept
			if err := txStore.UpdateSecretBinding(ctx, current, change); err != nil {
				return err
			}
			changed = []model.SecretBinding{*current}
		} else {
			if changed, err = txStore.DeleteSecretBinding(ctx, secret.ProjectID, current.ID, change); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
		if s.sandboxes != nil && len(changed) > 0 {
			if err := s.sandboxes.MarkBindingHoldersDirtyTx(ctx, txStore, txDB, secret.ProjectID, changed); err != nil {
				return err
			}
		}
	}
	return nil
}

// withoutRoutes is routes less those in drop, compared by canonical form, and
// whether any went.
func withoutRoutes(routes, drop []model.SecretRoute) ([]model.SecretRoute, bool) {
	dropped := make(map[string]bool, len(drop))
	for _, route := range drop {
		dropped[secretroute.Format(route)] = true
	}
	var kept []model.SecretRoute
	for _, route := range routes {
		if !dropped[secretroute.Format(route)] {
			kept = append(kept, route)
		}
	}
	return kept, len(kept) < len(routes)
}
