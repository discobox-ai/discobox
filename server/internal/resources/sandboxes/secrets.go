package sandboxes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/id"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/secretformat"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

const defaultSentinelFormat = "{alnum:48}"

// prepareSandboxSecrets resolves each secret input to a project secret (creating
// an anonymous secret for inline values), mints a sentinel placeholder from the
// secret's format, and returns the assignment rows to persist after the sandbox
// is created. The sentinel travels to the sandbox through the secrets channel
// (SandboxSecret rows -> CreateOptions.SecretEnv), never through sandbox.Env.
func (s *Service) prepareSandboxSecrets(ctx context.Context, projectID string, sandbox *model.Sandbox, inputs []services.SandboxSecretInput) ([]*model.SandboxSecret, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	seen := map[string]struct{}{}
	assignments := make([]*model.SandboxSecret, 0, len(inputs))
	for _, input := range inputs {
		env := strings.TrimSpace(input.Env)
		if env == "" || strings.ContainsAny(env, "=\x00") {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox secret requires a valid environment variable name")
		}
		if _, dup := seen[env]; dup {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("duplicate sandbox secret for environment variable %q", env))
		}
		seen[env] = struct{}{}

		secretID, format, err := s.resolveSecretForInput(ctx, projectID, input)
		if err != nil {
			return nil, err
		}
		sentinel, err := mintSentinel(format)
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, &model.SandboxSecret{
			ProjectID: projectID,
			SandboxID: sandbox.ID,
			SecretID:  secretID,
			EnvName:   env,
			Sentinel:  sentinel,
		})
	}
	return assignments, nil
}

// applyHarnessConfigSecrets materializes a harness config's secret bindings into
// sentinel assignments and enforces that every declared required secret is
// satisfied. inlineEnvs are env vars already bound per-sandbox by the create
// request; an inline secret wins over a binding for the same env. It returns
// the assignment rows to persist; sentinels reach the sandbox through the
// secrets channel, never sandbox.Env.
func (s *Service) applyHarnessConfigSecrets(ctx context.Context, projectID string, sandbox *model.Sandbox, harnessConfigID string, inlineEnvs map[string]struct{}) ([]*model.SandboxSecret, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, mapAPIError(err, "harness config not found")
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, err
	}
	boundByEnv := make(map[string]struct{}, len(bindings))
	for _, b := range bindings {
		boundByEnv[b.EnvName] = struct{}{}
	}

	// A required declared secret must be satisfied by an inline secret, an
	// harness-config binding, or a literal sandbox env var. OneOfGroup members are
	// satisfied collectively (at least one present).
	missing := missingRequiredSecrets(config.Secrets, func(name string) bool {
		if _, ok := inlineEnvs[name]; ok {
			return true
		}
		if _, ok := boundByEnv[name]; ok {
			return true
		}
		_, ok := sandbox.Env[name]
		return ok
	})
	if len(missing) > 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("harness config %q requires secrets with no bound value: %s", config.Slug, strings.Join(missing, ", ")))
	}

	assignments := make([]*model.SandboxSecret, 0, len(bindings))
	for _, b := range bindings {
		if _, ok := inlineEnvs[b.EnvName]; ok {
			continue // an explicit per-sandbox secret wins over the binding
		}
		secret, err := s.store.GetSecret(ctx, projectID, b.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // secret deleted out from under the binding; skip
			}
			return nil, err
		}
		sentinel, err := mintSentinel(s.secretFormat(ctx, secret))
		if err != nil {
			return nil, err
		}
		assignments = append(assignments, &model.SandboxSecret{
			ProjectID: projectID,
			SandboxID: sandbox.ID,
			SecretID:  secret.ID,
			EnvName:   b.EnvName,
			Sentinel:  sentinel,
		})
	}
	return assignments, nil
}

// applyPreviousConfigureSecrets injects the secrets a previous configure run
// created into a harnessMode=config sandbox, under
// harness.ConfigurePreviousEnvPrefix + the bound env name. The configure command
// uses them to verify and keep an existing credential instead of re-prompting.
//
// Only the flow's own secrets are offered: a secret the user bound by hand is
// theirs, not the configure flow's to replay. The values are sentinels like any
// other sandbox secret, so the credential itself stays in the control plane and
// resolves only while a live grant covers it — a revoked credential simply fails
// the configure command's verification. Required-secret enforcement stays off in
// config mode: this flow is how those secrets come to exist.
func (s *Service) applyPreviousConfigureSecrets(ctx context.Context, projectID string, sandbox *model.Sandbox, harnessConfigID string) ([]*model.SandboxSecret, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, mapAPIError(err, "harness config not found")
	}
	if len(config.ConfiguredSecretIDs) == 0 {
		return nil, nil
	}
	configured := make(map[string]struct{}, len(config.ConfiguredSecretIDs))
	for _, secretID := range config.ConfiguredSecretIDs {
		configured[strings.TrimSpace(secretID)] = struct{}{}
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, err
	}
	assignments := make([]*model.SandboxSecret, 0, len(bindings))
	for _, b := range bindings {
		if _, ok := configured[b.SecretID]; !ok {
			continue
		}
		secret, err := s.store.GetSecret(ctx, projectID, b.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // secret deleted out from under the binding; skip
			}
			return nil, err
		}
		sentinel, err := mintSentinel(s.secretFormat(ctx, secret))
		if err != nil {
			return nil, err
		}
		env := harness.ConfigurePreviousEnvPrefix + b.EnvName
		assignments = append(assignments, &model.SandboxSecret{
			ProjectID: projectID,
			SandboxID: sandbox.ID,
			SecretID:  secret.ID,
			EnvName:   env,
			Sentinel:  sentinel,
		})
	}
	return assignments, nil
}

// missingRequiredSecrets reports the unsatisfied required secret requirements of
// a harness config, given a predicate that reports whether a single env var is
// satisfied. Ungrouped required secrets each must be satisfied. Required secrets
// sharing a OneOfGroup form an at-least-one requirement: the group is satisfied
// when any member is present, otherwise it is reported as "one of: A, B". The
// returned list is deterministically ordered.
func missingRequiredSecrets(decls []model.HarnessConfigSecret, satisfied func(name string) bool) []string {
	var missing []string
	groupMembers := map[string][]string{}
	var groupOrder []string
	for _, decl := range decls {
		if !decl.Required {
			continue
		}
		if decl.OneOfGroup == "" {
			if !satisfied(decl.Name) {
				missing = append(missing, decl.Name)
			}
			continue
		}
		if _, seen := groupMembers[decl.OneOfGroup]; !seen {
			groupOrder = append(groupOrder, decl.OneOfGroup)
		}
		groupMembers[decl.OneOfGroup] = append(groupMembers[decl.OneOfGroup], decl.Name)
	}
	sort.Strings(missing)
	for _, group := range groupOrder {
		members := groupMembers[group]
		if slices.ContainsFunc(members, satisfied) {
			continue
		}
		sort.Strings(members)
		missing = append(missing, "one of: "+strings.Join(members, ", "))
	}
	return missing
}

// resolveSecretForInput returns the secret ID and format template for one input,
// creating an anonymous secret when an inline value is supplied.
func (s *Service) resolveSecretForInput(ctx context.Context, projectID string, input services.SandboxSecretInput) (secretID string, format string, err error) {
	refID := strings.TrimSpace(input.SecretId.Or(""))
	value := input.Value.Or("")
	switch {
	case refID != "" && strings.TrimSpace(value) != "":
		return "", "", apperrors.NewStatusError(http.StatusBadRequest, "sandbox secret must set exactly one of secretId or value")
	case refID != "":
		secret, err := s.store.GetSecret(ctx, projectID, refID)
		if err != nil {
			return "", "", mapAPIError(err, "secret not found")
		}
		return secret.ID, s.secretFormat(ctx, secret), nil
	case strings.TrimSpace(value) != "":
		secret, err := s.createAnonymousSecret(ctx, projectID, value, strings.TrimSpace(input.Host.Or("")))
		if err != nil {
			return "", "", err
		}
		return secret.ID, secret.Format, nil
	default:
		return "", "", apperrors.NewStatusError(http.StatusBadRequest, "sandbox secret must set secretId or value")
	}
}

// secretFormat returns the secret's stored format, falling back to inferring one
// from the decrypted bearer value so referenced secrets without a format still
// mint a convincing sentinel.
func (s *Service) secretFormat(ctx context.Context, secret *model.Secret) string {
	if strings.TrimSpace(secret.Format) != "" {
		return secret.Format
	}
	if secret.Type == model.SecretTypeBearer {
		if val, err := s.store.OpenSecretValue(ctx, secret); err == nil && val != nil {
			if token := strings.TrimSpace(val.Token); token != "" {
				format, _ := secretformat.Describe(token)
				return format
			}
		}
	}
	return defaultSentinelFormat
}

func (s *Service) createAnonymousSecret(ctx context.Context, projectID, value, host string) (*model.Secret, error) {
	secretID, err := id.New(id.PrefixSecret)
	if err != nil {
		return nil, err
	}
	format, hostHint := secretformat.Describe(value)
	if host == "" {
		host = hostHint
	}
	//nolint:gosec // Secret values are intentionally marshaled before store encryption.
	valueBytes, err := json.Marshal(model.SecretValue{Token: value})
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
	}
	secret := &model.Secret{
		ID:              secretID,
		ProjectID:       projectID,
		Name:            "sandbox-secret-" + secretID,
		Type:            model.SecretTypeBearer,
		Host:            host,
		UniqueKey:       secretID, // keeps anonymous rows out of the (project,type,host) uniqueness domain
		Anonymous:       true,
		Format:          format,
		DefaultGrantTTL: defaultAnonymousGrantTTLSeconds,
		EncryptedValue:  valueBytes,
	}
	if err := s.store.CreateSecret(ctx, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

const defaultAnonymousGrantTTLSeconds = 3600

// AssignSandboxHarnessSecrets ensures the given harness config's secret bindings are
// materialized for a running sandbox and returns the resulting env->sentinel map
// for the caller to inject into a per-invocation exec/terminal environment.
//
// Assignment is flat per env: if an env var is already assigned (by the primary
// harness or an earlier assignment), that sentinel is reused (first-assigner wins)
// even if this harness binds the env to a different secret. Newly minted sentinels
// are pushed to the running sandbox's proxy immediately so they resolve without a
// restart. Required declared secrets with no binding are rejected.
func (s *Service) AssignSandboxHarnessSecrets(ctx context.Context, projectID, sandboxID, harnessConfigID string) (map[string]string, error) {
	sandboxModel, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, mapAPIError(err, "sandbox not found")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, mapAPIError(err, "harness config not found")
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, projectID, harnessConfigID)
	if err != nil {
		return nil, err
	}
	existing, err := s.store.ListSandboxSecrets(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	envSentinel := make(map[string]string, len(existing))
	for _, assignment := range existing {
		envSentinel[assignment.EnvName] = assignment.Sentinel
	}

	result := make(map[string]string, len(bindings))
	created := false
	for _, binding := range bindings {
		if sentinel, ok := envSentinel[binding.EnvName]; ok {
			result[binding.EnvName] = sentinel // reuse the existing assignment (first wins)
			continue
		}
		secret, err := s.store.GetSecret(ctx, projectID, binding.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue // bound secret was deleted; leave the env unset
			}
			return nil, err
		}
		sentinel, err := mintSentinel(s.secretFormat(ctx, secret))
		if err != nil {
			return nil, err
		}
		if err := s.store.CreateSandboxSecret(ctx, &model.SandboxSecret{
			ProjectID: projectID,
			SandboxID: sandboxID,
			SecretID:  secret.ID,
			EnvName:   binding.EnvName,
			Sentinel:  sentinel,
		}); err != nil {
			return nil, err
		}
		envSentinel[binding.EnvName] = sentinel
		result[binding.EnvName] = sentinel
		created = true
	}

	// A required declared secret must be satisfied by an assignment. OneOfGroup
	// members are satisfied collectively (at least one present).
	missing := missingRequiredSecrets(config.Secrets, func(name string) bool {
		_, ok := envSentinel[name]
		return ok
	})
	if len(missing) > 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("harness config %q requires secrets with no bound value: %s", config.Slug, strings.Join(missing, ", ")))
	}

	if created {
		if err := s.pushSandboxSentinels(ctx, sandboxModel); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// pushSandboxSentinels re-registers the running sandbox's full sentinel set with
// its provider (and thus the proxy) so newly assigned secrets resolve live.
func (s *Service) pushSandboxSentinels(ctx context.Context, sandboxModel *model.Sandbox) error {
	if s.sandboxProviders == nil {
		return fmt.Errorf("sandbox provider manager is required")
	}
	assignments, err := s.store.ListSandboxSecrets(ctx, sandboxModel.ProjectID, sandboxModel.ID)
	if err != nil {
		return err
	}
	sentinels := make([]string, 0, len(assignments))
	secretEnv := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		sentinels = append(sentinels, assignment.Sentinel)
		secretEnv[assignment.EnvName] = assignment.Sentinel
	}
	provider, err := s.sandboxProviders.ResolveForSandbox(ctx, sandboxModel)
	if err != nil {
		return err
	}
	if _, _, err := provider.Update(ctx, SandboxRef{ProjectID: sandboxModel.ProjectID, SandboxID: sandboxModel.ID}, sandboxModel.ProviderState, UpdateOptions{Sentinels: sentinels, SecretEnv: secretEnv}); err != nil {
		return err
	}
	return nil
}

func mintSentinel(format string) (string, error) {
	tmpl, err := secretformat.Parse(strings.TrimSpace(format))
	if err != nil {
		tmpl, err = secretformat.Parse(defaultSentinelFormat)
		if err != nil {
			return "", err
		}
	}
	return tmpl.Generate()
}
