package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
	"gorm.io/gorm"
)

// A sandbox's sentinels are its effective use bindings, materialized (ADR
// 26-10-27-880 §2): one SandboxSecret row per variable, naming the binding
// that delivers it. materializeSandboxSecrets makes the rows match the
// bindings; everything that changes a binding marks SentinelsResourceType for
// the sandboxes it covers, whose reconciler materializes and pushes.

// inlineSandboxBindings makes, for each inline secret a create carries, an
// anonymous secret holding its value and a use binding of it on the new
// sandbox in the variable named. The secret's route is the host given, else
// `*` (ADR 26-10-27-939 §2). Neither is stored here: both are written with the
// sandbox, so a create that fails leaves no value behind.
func inlineSandboxBindings(ctx context.Context, projectID string, sandbox *model.Sandbox, inputs []services.SandboxSecretInput) ([]*model.Secret, []*model.SecretBinding, error) {
	if len(inputs) == 0 {
		return nil, nil, nil
	}
	seen := map[string]struct{}{}
	principal, _ := auth.PrincipalFromContext(ctx)
	secrets := make([]*model.Secret, 0, len(inputs))
	bindings := make([]*model.SecretBinding, 0, len(inputs))
	for _, input := range inputs {
		env := strings.TrimSpace(input.Env)
		if !services.EnvVarNamePattern.MatchString(env) {
			return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox secret requires a valid environment variable name")
		}
		if _, dup := seen[env]; dup {
			return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("duplicate sandbox secret for environment variable %q", env))
		}
		seen[env] = struct{}{}
		if strings.TrimSpace(input.Value) == "" {
			return nil, nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("sandbox secret %s has no value", env))
		}
		secret, err := anonymousSecret(projectID, input.Value, strings.TrimSpace(input.Host.Or("")))
		if err != nil {
			return nil, nil, err
		}
		secrets = append(secrets, secret)
		bindings = append(bindings, &model.SecretBinding{
			ProjectID:  projectID,
			SecretID:   secret.ID,
			TargetKind: model.SecretBindingTargetSandbox,
			TargetID:   sandbox.ID,
			Use:        true,
			EnvName:    env,
			CreatedBy:  principal.Actor(),
		})
	}
	return secrets, bindings, nil
}

// anonymousSecret is an inline value as a secret of its own, sent to host
// alone — a route to that exact host — or anywhere when none is given.
func anonymousSecret(projectID, value, host string) (*model.Secret, error) {
	secretID, err := id.New(id.PrefixSecret)
	if err != nil {
		return nil, err
	}
	//nolint:gosec // Secret values are intentionally marshaled before store encryption.
	valueBytes, err := json.Marshal(model.SecretValue{Token: value})
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
	}
	route := model.SecretRoute{Hosts: []string{secretroute.AnyHost}}
	if host != "" {
		route.Hosts = []string{host}
	}
	routes, err := model.NormalizeRoutes([]model.SecretRoute{route})
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	secret := &model.Secret{
		ID:        secretID,
		ProjectID: projectID,
		Name:      "sandbox-secret-" + secretID,
		Type:      model.SecretTypeToken,
		Routes:    routes,
		Anonymous: true,
		// The value lives as long as the sandbox it was given to, and goes
		// with it: there is no limit to hold its binding to.
		MaxBindingTTL:  0,
		EncryptedValue: valueBytes,
	}
	return secret, nil
}

// materializeSandboxSecrets makes a sandbox's sentinel rows its effective use
// bindings, reporting whether any row changed.
//
// The sentinel is kept wherever it can be: it is a per-sandbox opaque string
// whose only meaning is the row it maps through, so repointing that row hands
// the sandbox the new credential with nothing to re-deliver and no restart. It
// is only re-minted when the new secret's format differs, because the sentinel
// byte-mimics the credential it stands in for — and the string in a harness's
// credential file is dead the moment it is replaced. That is why a row
// records the format it was minted from.
func materializeSandboxSecrets(ctx context.Context, st *store.Store, sb *model.Sandbox) (bool, error) {
	effective, err := st.ResolveSandboxBindings(ctx, sb, time.Now().UTC())
	if err != nil {
		return false, err
	}
	desired := map[string]model.SecretBinding{}
	for _, applied := range effective.Use {
		desired[applied.Binding.EnvName] = applied.Binding
	}
	rows, err := st.ListSandboxSecrets(ctx, sb.ProjectID, sb.ID)
	if err != nil {
		return false, err
	}
	changed := false
	for i := range rows {
		row := &rows[i]
		binding, wanted := desired[row.EnvName]
		if !wanted {
			if err := st.DeleteSandboxSecret(ctx, row.ID); err != nil {
				return false, err
			}
			changed = true
			continue
		}
		delete(desired, row.EnvName)
		if row.SecretID == binding.SecretID && row.BindingID == binding.ID && row.Raw == binding.Raw {
			continue
		}
		if row.SecretID != binding.SecretID {
			secret, err := st.GetSecret(ctx, sb.ProjectID, binding.SecretID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, err
			}
			format := st.SentinelFormat(ctx, secret)
			// A row minted before formats were recorded keeps its sentinel:
			// what it was minted from is unknown, and guessing wrong costs a
			// live harness session.
			if row.Format != "" && row.Format != format {
				if row.Sentinel, err = secretformat.MintSentinel(format); err != nil {
					return false, err
				}
			}
			row.SecretID, row.Format = secret.ID, format
		}
		row.BindingID, row.Raw = binding.ID, binding.Raw
		if err := st.UpdateSandboxSecret(ctx, row); err != nil {
			return false, err
		}
		changed = true
	}
	envs := make([]string, 0, len(desired))
	for env := range desired {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	for _, env := range envs {
		binding := desired[env]
		secret, err := st.GetSecret(ctx, sb.ProjectID, binding.SecretID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		row, err := st.NewSandboxBinding(ctx, sb.ProjectID, sb.ID, env, secret)
		if err != nil {
			return false, err
		}
		row.BindingID, row.Raw = binding.ID, binding.Raw
		if err := st.CreateSandboxSecret(ctx, row); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// MaterializeSandboxSecretsTx makes one sandbox's sentinels its bindings, in
// the caller's transaction (materializeSandboxSecrets).
func (s *Service) MaterializeSandboxSecretsTx(ctx context.Context, txStore *store.Store, projectID, sandboxID string) error {
	sb, err := txStore.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	_, err = materializeSandboxSecrets(ctx, txStore, sb)
	return err
}

// AssignSandboxHarnessSecrets gives a running sandbox the credentials a
// harness config binds, for a harness run in it beside its own, and returns
// the env->sentinel map for the caller to inject into that invocation's
// environment.
//
// Nothing is copied: the harness config is attached to the sandbox
// (store.AttachSandboxHarnessConfig), and its bindings apply there after the
// sandbox's own harness config's and before the project's (ADR 26-10-27-880
// §2). A variable the sandbox already holds keeps its credential; removing or
// narrowing the harness config's binding later reaches the sandbox as it
// reaches every sandbox running the config. Attaching is a person's: a
// discobox may only use a harness config already attached. The sandbox is
// materialized and its sentinels pushed in place, so they resolve without a
// restart. A declared secret nothing delivers is not refused: the harness
// starts signed out, and is signed in inside the box (ADR 26-11-02-152 §1).
func (s *Service) AssignSandboxHarnessSecrets(ctx context.Context, projectID, sandboxID, harnessConfigID string) (map[string]string, error) {
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, apperrors.NotFound(err, "harness config not found")
	}
	now := time.Now().UTC()
	harnessBindings, err := s.store.ResolveBindings(ctx, projectID,
		store.BindingTargetsAt(projectID, model.SecretBindingTargetHarnessConfig, config.ID), now)
	if err != nil {
		return nil, err
	}
	alreadyCovered := false
	for _, target := range store.SandboxBindingTargets(sandboxModel) {
		if target.Kind == model.SecretBindingTargetHarnessConfig && target.ID == config.ID {
			alreadyCovered = true
		}
	}
	if !alreadyCovered {
		if principal, _ := auth.PrincipalFromContext(ctx); principal.Type == auth.PrincipalTypeSandbox {
			return nil, apperrors.NewStatusError(http.StatusForbidden,
				fmt.Sprintf("harness config %q is not attached to this discobox, and a discobox does not attach one; ask a person to", config.Slug))
		}
	}
	var rows []model.SandboxSecret
	changed := false
	if err := s.store.Transaction(ctx, func(txStore *store.Store, _ *gorm.DB) error {
		if !alreadyCovered {
			if _, err := txStore.AttachSandboxHarnessConfig(ctx, projectID, sandboxID, config.ID); err != nil {
				return err
			}
		}
		current, err := txStore.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return err
		}
		if changed, err = materializeSandboxSecrets(ctx, txStore, current); err != nil {
			return err
		}
		rows, err = txStore.ListSandboxSecrets(ctx, projectID, sandboxID)
		return err
	}); err != nil {
		return nil, err
	}
	envSentinel := make(map[string]string, len(rows))
	for _, row := range rows {
		// A raw binding is delivered by the sandbox agent to every process it
		// starts; its sentinel is only the pool's handle (ADR 26-10-27-840).
		if row.Raw == "" {
			envSentinel[row.EnvName] = row.Sentinel
		}
	}
	result := map[string]string{}
	for _, applied := range harnessBindings.Use {
		if sentinel, ok := envSentinel[applied.Binding.EnvName]; ok {
			result[applied.Binding.EnvName] = sentinel
		}
	}
	if changed {
		if err := s.pushSandboxSentinels(ctx, sandboxModel); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// pushSandboxSentinels re-registers the running sandbox's full sentinel set,
// each with its route lists, with its provider (and thus the proxy) so what it
// holds now resolves live, and what it no longer holds does not.
//
// Push even when every sentinel was kept: the proxy caches a resolved value
// per sentinel, and applying a sentinel set rebuilds the swapper, which drops
// that cache.
func (s *Service) pushSandboxSentinels(ctx context.Context, sandboxModel *model.Sandbox) error {
	if s.sandboxProviders == nil {
		return fmt.Errorf("sandbox provider manager is required")
	}
	assignments, err := s.store.ListSandboxSecrets(ctx, sandboxModel.ProjectID, sandboxModel.ID)
	if err != nil {
		return err
	}
	sentinels, secretEnv, rawSecrets := sentinelDelivery(assignments)
	if sentinels == nil {
		// An empty set, not none: it is what removes the last sentinel.
		sentinels = []string{}
	}
	if secretEnv == nil {
		// Likewise an empty environment: without it the pool keeps the last
		// one it was sent, and a variable whose binding went, or turned raw,
		// keeps its sentinel in the sandbox, where it shadows a raw value's
		// env delivery with the pool's handle (ADR 26-10-27-840 §3).
		secretEnv = map[string]string{}
	}
	sentinelRoutes, err := s.store.SentinelRouteLists(ctx, sandboxModel.ProjectID, assignments)
	if err != nil {
		return err
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sandboxModel)
	if err != nil {
		return err
	}
	if _, _, err := provider.Update(ctx, SandboxRef{ProjectID: sandboxModel.ProjectID, SandboxID: sandboxModel.ID}, sandboxModel.ProviderState, UpdateOptions{Sentinels: sentinels, SentinelRoutes: sentinelRoutes, RawSecrets: rawSecrets, SecretEnv: secretEnv}); err != nil {
		return err
	}
	return markSentinelsIfStale(ctx, s.store, s.engine, sandboxModel, sentinelRoutes)
}

// sentinelDelivery splits a sandbox's rows into what its pool is sent: the
// sentinels delivered into it and swapped, its secret environment, and its raw
// bindings (ADR 26-10-27-840), whose sentinel is only the pool's handle for
// the value and is delivered nowhere. Each is nil when there are none.
func sentinelDelivery(assignments []model.SandboxSecret) ([]string, map[string]string, map[string]RawSecret) {
	var sentinels []string
	var secretEnv map[string]string
	var raw map[string]RawSecret
	for _, assignment := range assignments {
		if assignment.Raw != "" {
			if raw == nil {
				raw = map[string]RawSecret{}
			}
			raw[assignment.Sentinel] = RawSecret{EnvName: assignment.EnvName, Delivery: assignment.Raw}
			continue
		}
		sentinels = append(sentinels, assignment.Sentinel)
		if secretEnv == nil {
			secretEnv = map[string]string{}
		}
		secretEnv[assignment.EnvName] = assignment.Sentinel
	}
	return sentinels, secretEnv, raw
}

// MarkSecretHoldersDirtyTx marks the sentinel delivery of every sandbox
// holding a secret for reconcile, in the transaction that changed the secret's
// routes or removed it (ADR 26-10-02-924). SentinelReconciler re-sends each
// sandbox's sentinel set with their current route lists, and retries a
// delivery its pool did not take, so the routes as written reach every
// holder.
func (s *Service) MarkSecretHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, secretID string) error {
	sandboxIDs, err := txStore.ListSandboxIDsHoldingSecret(ctx, projectID, secretID)
	if err != nil {
		return err
	}
	return markSentinelsTx(ctx, s.engine, txDB, projectID, sandboxIDs)
}

// MarkBindingHoldersDirtyTx marks the sentinel delivery of every sandbox the
// bindings' targets cover, in the transaction that made, changed or removed
// them: SentinelReconciler materializes each sandbox's bindings again and
// sends its pool the result.
func (s *Service) MarkBindingHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID string, bindings []model.SecretBinding) error {
	return markBindingHoldersTx(ctx, s.engine, txStore, txDB, projectID, bindings)
}

// markBindingHoldersTx is MarkBindingHoldersDirtyTx for a caller holding
// the engine rather than the service.
func markBindingHoldersTx(ctx context.Context, engine *reconcile.Engine, txStore *store.Store, txDB *gorm.DB, projectID string, bindings []model.SecretBinding) error {
	seen := map[string]bool{}
	var sandboxIDs []string
	for _, binding := range bindings {
		target := binding.TargetKind + "\x00" + binding.TargetID
		if seen[target] {
			continue
		}
		seen[target] = true
		covered, err := txStore.ListSandboxIDsCoveredBy(ctx, projectID, binding.TargetKind, binding.TargetID)
		if err != nil {
			return err
		}
		sandboxIDs = append(sandboxIDs, covered...)
	}
	return markSentinelsTx(ctx, engine, txDB, projectID, sandboxIDs)
}

// markSentinelsTx marks each sandbox's sentinel delivery once.
func markSentinelsTx(ctx context.Context, engine *reconcile.Engine, txDB *gorm.DB, projectID string, sandboxIDs []string) error {
	if engine == nil {
		return nil
	}
	seen := map[string]bool{}
	for _, sandboxID := range sandboxIDs {
		if seen[sandboxID] {
			continue
		}
		seen[sandboxID] = true
		if err := engine.MarkDirtyTx(ctx, txDB, SentinelsResourceType, SandboxDirtyID(projectID, sandboxID)); err != nil {
			return err
		}
	}
	return nil
}
