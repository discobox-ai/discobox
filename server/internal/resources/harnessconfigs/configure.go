package harnessconfigs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"strings"
	"time"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/discobox-ai/x/id"
)

// HarnessConfigResourceType is the reconcile engine resource type for harness
// configs. The reconciler is a janitor: it reaps configure sandboxes that were
// started but never committed.
const HarnessConfigResourceType = "harnessConfig"

// configureTTL is how long an in-flight configure may sit uncommitted before the
// janitor reaps its sandbox. Configure is interactive, so this is generous: it
// only needs to catch a client that walked away.
const configureTTL = time.Hour

// execSettleTimeout bounds how long a finished one-shot exec may take to record
// its exit status.
const execSettleTimeout = 15 * time.Second

// errConfigureRunning marks a commit that arrived before the configure command
// finished. It is not a failure: the flow stays in flight so the caller can
// finish and commit again.
var errConfigureRunning = errors.New("the configure flow is still running")

// configureOutput is the JSON a harness's configure command leaves at
// harness.ConfigureOutputPath. The same shape is seeded back in as the previous
// configuration (harness.ConfigurePreviousConfigPath) so a configure command can
// parse its own prior output and pre-fill from it — but the seed carries no
// secret values, only the metadata saying which secrets exist. Their values are
// offered as PREV_-prefixed sentinel env vars instead
// (harness.ConfigurePreviousEnvPrefix).
type configureOutput struct {
	Secrets []configureSecret         `json:"secrets"`
	Files   []model.HarnessConfigFile `json:"files"`
}

type configureSecret struct {
	EnvName string `json:"envName"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Host    string `json:"host,omitempty"`
	// UsePrevious keeps the secret a previous configure run stored for this env
	// name, leaving Value empty. It is how a configure command says "the existing
	// credential is still good" without handling the credential at all.
	UsePrevious bool            `json:"usePrevious,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
}

// SandboxRuntime is the slice of sandbox behavior the configure flow needs: it
// runs an ephemeral sandbox, reaches its agent, and removes it when done.
//
// AcquireSandboxHTTPClient authorizes the caller's scopes, so every agent call
// the configure flow makes happens inside a user request with that user's
// credentials. Nothing here talks to a sandbox on its own authority.
type SandboxRuntime interface {
	CreateSandbox(ctx context.Context, projectID string, input services.CreateSandboxBody) (*model.Sandbox, error)
	DeleteSandbox(ctx context.Context, projectID, sandboxID string) error
	AcquireSandboxHTTPClient(ctx context.Context, projectID, sandboxID string, scopes []string) (*services.HTTPClientLease, *model.Sandbox, error)
}

// Dirtier schedules reconciliation.
type Dirtier interface {
	MarkDirtyAt(ctx context.Context, resourceType, id string, at time.Time) error
}

// SetSandboxRuntime installs the sandbox dependency used by the configure flow.
func (s *Service) SetSandboxRuntime(runtime SandboxRuntime) { s.sandboxes = runtime }

// SetDirtier installs the reconcile hook used to reap abandoned configures.
func (s *Service) SetDirtier(dirtier Dirtier) { s.dirtier = dirtier }

// ConfigureHarnessConfig launches the harness's configure sandbox and returns it.
// The sandbox comes up idle: in config mode the sandbox-agent defers the primary
// terminal until it is attached, so the caller must call
// AttachHarnessConfigConfigure (which seeds the previous configuration) and then
// attach to the virtual primary exec, which launches the configure command.
//
// Re-configuring is always allowed. An in-flight configure is clobbered rather
// than rejected, so an abandoned attempt can never wedge the harness.
func (s *Service) ConfigureHarnessConfig(ctx context.Context, projectID, configID string) (*model.Sandbox, error) {
	if s.sandboxes == nil || s.dirtier == nil {
		return nil, errors.New("harness configure requires the sandbox runtime and reconcile engine")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	if strings.TrimSpace(config.Image) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "harness config has no image to configure")
	}
	// Stated in terms of the missing command rather than the slug: the reserved
	// `shell` built-in is a login shell with no credentials to collect (ADR
	// 0025 §2), and so is any other image that declares no configure command.
	if len(config.ConfigCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q has nothing to configure: its image declares no configure command", config.Slug))
	}
	if previous := strings.TrimSpace(config.ConfigureSandboxID); previous != "" {
		if err := s.sandboxes.DeleteSandbox(ctx, projectID, previous); err != nil && !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "failed to delete superseded configure sandbox",
				"harnessConfigId", config.ID, "sandboxId", previous, "error", err)
		}
	}

	sandboxName, err := configureSandboxName()
	if err != nil {
		return nil, err
	}
	var input services.CreateSandboxBody
	input.Config.Name = sandboxName
	input.Config.SetHarnessConfigId(serverapi.NewOptString(config.ID))
	input.Config.SetHarnessMode(serverapi.NewOptSandboxCreateConfigHarnessMode(serverapi.SandboxCreateConfigHarnessModeConfig))
	input.Config.SetImage(serverapi.NewOptString(config.Image))
	// Run the configure command as a real, non-root account. A run sandbox
	// mirrors the caller's own user; this one has no caller to mirror, so the
	// flow names the account and boot creates it. Configuring as root would
	// exercise the harness CLI under an identity no run sandbox ever uses.
	configureUser := serverapi.SandboxUser{}
	configureUser.SetName(serverapi.NewOptString(harness.ConfigureUserName))
	configureUser.SetUID(serverapi.NewOptInt64(harness.ConfigureUserUID))
	configureUser.SetGid(serverapi.NewOptInt64(harness.ConfigureUserGID))
	input.Config.SetUser(serverapi.NewOptSandboxUser(configureUser))

	created, err := s.sandboxes.CreateSandbox(ctx, projectID, input)
	if err != nil {
		return nil, err
	}

	config.ConfigureSandboxID = created.ID
	config.ConfigureError = ""
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		// The sandbox is untracked if we cannot record it, so do not leak it.
		_ = s.sandboxes.DeleteSandbox(context.WithoutCancel(ctx), projectID, created.ID)
		return nil, err
	}
	// Reap it if the caller never commits.
	if err := s.dirtier.MarkDirtyAt(ctx, HarnessConfigResourceType, config.ID, time.Now().Add(configureTTL)); err != nil {
		return nil, err
	}
	return created, nil
}

// AttachHarnessConfigConfigure seeds the previous configuration into the
// in-flight configure sandbox, for the harness's configure command to pre-fill
// from. It must be called before attaching to the primary terminal, which is
// what launches that command.
//
// The seed names the secrets a previous run created but carries no value for
// any of them, so it cannot be a way to read a secret. The values are offered
// separately as PREV_-prefixed sentinels, which resolve only while a live grant
// covers them — enforcement stays at use, in ResolveSandboxSecret, rather than
// being repeated here. See ADR 0009.
func (s *Service) AttachHarnessConfigConfigure(ctx context.Context, projectID, configID string) error {
	if s.sandboxes == nil {
		return errors.New("harness configure requires the sandbox runtime")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return apiError(err, "harness config not found")
	}
	sandboxID := strings.TrimSpace(config.ConfigureSandboxID)
	if sandboxID == "" {
		return apperrors.NewStatusError(http.StatusConflict, "no configure flow is running for this harness")
	}
	previous, err := s.previousConfiguration(ctx, config)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	run, release, err := s.sandboxAgentClient(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	defer release()
	return seedFile(ctx, run, harness.ConfigurePreviousConfigPath, payload)
}

// previousConfiguration rebuilds the configure output that produced the harness's
// current configuration, in the same shape the configure command writes, minus
// every secret value: it says which secrets exist, not what they are. The values
// ride in as PREV_-prefixed sentinels (applyPreviousConfigureSecrets), so no
// credential is ever written to a file inside the sandbox.
func (s *Service) previousConfiguration(ctx context.Context, config *model.HarnessConfig) (*configureOutput, error) {
	out := &configureOutput{Files: config.ConfiguredFiles, Secrets: []configureSecret{}}
	if out.Files == nil {
		out.Files = []model.HarnessConfigFile{}
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, config.ProjectID, config.ID)
	if err != nil {
		return nil, err
	}
	configured := map[string]struct{}{}
	for _, secretID := range config.ConfiguredSecretIDs {
		configured[strings.TrimSpace(secretID)] = struct{}{}
	}
	for _, binding := range bindings {
		if _, ok := configured[binding.SecretID]; !ok {
			// Only hand back what this configure flow created; a secret the user
			// bound by hand is theirs, not the flow's to replay.
			continue
		}
		secret, err := s.store.GetSecret(ctx, config.ProjectID, binding.SecretID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return nil, err
		}
		// Metadata only. Whether the credential is still usable is not decided
		// here: its sentinel resolves through the proxy only while a live grant
		// covers it, so a revoked credential fails the command's own verification
		// rather than needing a second, divergent grant check on this path.
		out.Secrets = append(out.Secrets, configureSecret{
			EnvName:     binding.EnvName,
			Name:        secret.Name,
			Type:        secret.Type,
			Host:        secret.Host,
			UsePrevious: true,
		})
	}
	return out, nil
}

// CommitHarnessConfigConfigure finishes an in-flight configure: it verifies the
// configure command actually exited 0, reads what it wrote, and applies it.
//
// The caller says when to commit, but not what happened — the exit status is read
// from the sandbox itself, so a client cannot mark a harness configured without
// having run the flow.
func (s *Service) CommitHarnessConfigConfigure(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	if s.sandboxes == nil {
		return nil, errors.New("harness configure requires the sandbox runtime")
	}
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	sandboxID := strings.TrimSpace(config.ConfigureSandboxID)
	if sandboxID == "" {
		return nil, apperrors.NewStatusError(http.StatusConflict, "no configure flow is running for this harness")
	}

	out, configureErr := s.collectConfigureOutput(ctx, projectID, sandboxID)
	if errors.Is(configureErr, errConfigureRunning) {
		// Not done yet: leave the sandbox and the in-flight state alone so the
		// caller can finish and commit again. Only a finished flow is a verdict.
		return nil, apperrors.NewStatusError(http.StatusConflict, configureErr.Error())
	}
	if configureErr == nil {
		configureErr = s.applyConfigureOutput(ctx, config, sandboxID, out)
	}

	config.ConfigureSandboxID = ""
	if configureErr != nil {
		config.ConfigureError = configureErr.Error()
		slog.WarnContext(ctx, "harness configure failed",
			"harnessConfigId", config.ID, "sandboxId", sandboxID, "error", configureErr)
	} else {
		config.Configured = true
		config.ConfigureError = ""
		slog.InfoContext(ctx, "harness configured", "harnessConfigId", config.ID, "sandboxId", sandboxID)
	}
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	// The configure sandbox is ephemeral: drop it whichever way the run went.
	if err := s.sandboxes.DeleteSandbox(ctx, projectID, sandboxID); err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.WarnContext(ctx, "failed to delete configure sandbox",
			"harnessConfigId", config.ID, "sandboxId", sandboxID, "error", err)
	}
	if configureErr != nil {
		return nil, apperrors.NewStatusError(http.StatusConflict, configureErr.Error())
	}
	return s.store.GetHarnessConfig(ctx, projectID, configID)
}

// collectConfigureOutput verifies the configure command exited cleanly and reads
// the result it wrote.
func (s *Service) collectConfigureOutput(ctx context.Context, projectID, sandboxID string) (*configureOutput, error) {
	run, release, err := s.sandboxAgentClient(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	defer release()

	// Read the primary's status by its real exec id. Resolving the virtual
	// "primary" id would relaunch a stopped primary — here that would restart the
	// configure command instead of observing that it finished.
	terminal, found, err := run.settledPrimaryTerminal(ctx)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("the configure flow was never started")
	}
	switch terminal.Status {
	case "exited":
		if terminal.ExitCode == nil || *terminal.ExitCode != 0 {
			return nil, fmt.Errorf("configure flow exited with code %d", derefExitCode(terminal.ExitCode))
		}
	case "failed", "lost":
		return nil, fmt.Errorf("configure flow %s: %s", terminal.Status, terminal.Error)
	default:
		return nil, errConfigureRunning
	}
	return readConfigureOutput(ctx, run)
}

// applyConfigureOutput records what the configure flow produced: its files land in
// ConfiguredFiles, and each declared secret is created, bound, and granted to this
// harness config, with the resulting IDs tracked so deconfigure removes exactly
// these. The grant is what makes the secret usable at run time — a binding alone
// is not a grant.
//
// A secret the command asked to keep (UsePrevious, or the PREV_ sentinel handed
// straight back) is carried over as-is: the existing secret row, binding, and
// grant survive, and nothing about the credential passes through this process.
func (s *Service) applyConfigureOutput(ctx context.Context, config *model.HarnessConfig, sandboxID string, out *configureOutput) error {
	if out == nil {
		return nil
	}
	previousByEnv, err := s.previousSecretIDsByEnv(ctx, config)
	if err != nil {
		return err
	}
	sentinelSecretIDs, err := s.previousSentinels(ctx, config.ProjectID, sandboxID)
	if err != nil {
		return err
	}
	createdSecretIDs := make([]string, 0, len(out.Secrets))
	for _, secret := range out.Secrets {
		envName := strings.TrimSpace(secret.EnvName)
		if !services.HarnessConfigEnvVarNamePattern.MatchString(envName) {
			return fmt.Errorf("configure output has invalid secret environment variable %q", secret.EnvName)
		}
		name := strings.TrimSpace(secret.Name)
		if name == "" {
			name = envName
		}
		// Keep the existing secret when the command said to, or when it handed a
		// PREV_ sentinel back as the value — the sentinel is not a credential, and
		// storing it would silently configure the harness with a dead one.
		var keptID string
		if secret.UsePrevious {
			keptID = previousByEnv[envName]
			if keptID == "" {
				return fmt.Errorf("configure output reuses %q, but no previously configured secret is bound to it", envName)
			}
		} else {
			keptID = matchPreviousSentinel(secret.Value, sentinelSecretIDs)
		}
		if keptID != "" {
			createdSecretIDs = append(createdSecretIDs, keptID)
			if err := s.store.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
				ProjectID:       config.ProjectID,
				HarnessConfigID: config.ID,
				EnvName:         envName,
				SecretID:        keptID,
			}); err != nil {
				return fmt.Errorf("bind kept secret %q: %w", envName, err)
			}
			continue
		}
		if len(bytes.TrimSpace(secret.Value)) == 0 {
			return fmt.Errorf("configure output secret %q has no value and does not reuse a previous one", envName)
		}
		// A new value for an env name this harness already binds updates that
		// secret in place, so its ID — and every sandbox sentinel keyed on it —
		// stays stable and running sandboxes resolve the new value without a new
		// grant or sentinel. Only a genuinely new env name mints a new secret.
		if existingID := previousByEnv[envName]; existingID != "" {
			existing, err := s.store.GetSecret(ctx, config.ProjectID, existingID)
			if err != nil {
				return fmt.Errorf("load configured secret %q for update: %w", envName, err)
			}
			hostChanged := existing.Host != secret.Host
			existing.Name = name
			existing.Type = strings.TrimSpace(secret.Type)
			existing.Host = secret.Host
			existing.EncryptedValue = []byte(secret.Value)
			if err := s.store.UpdateSecret(ctx, existing); err != nil {
				return fmt.Errorf("update configured secret %q: %w", envName, err)
			}
			createdSecretIDs = append(createdSecretIDs, existing.ID)
			// The binding already points here, but re-upsert so it stays
			// authoritative even if its stored fields drifted.
			if err := s.store.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
				ProjectID:       config.ProjectID,
				HarnessConfigID: config.ID,
				EnvName:         envName,
				SecretID:        existing.ID,
			}); err != nil {
				return fmt.Errorf("bind updated secret %q: %w", envName, err)
			}
			// A host change moves which requests the grant authorizes, so the
			// standing grant's host must follow the secret's or the value stops
			// resolving for the new host.
			if hostChanged {
				if err := s.updateConfiguredGrantHost(ctx, config, existing.ID, secret.Host); err != nil {
					return fmt.Errorf("update grant host for %q: %w", envName, err)
				}
			}
			continue
		}
		secretID, err := id.New(id.PrefixSecret)
		if err != nil {
			return err
		}
		created := &model.Secret{
			ID:        secretID,
			ProjectID: config.ProjectID,
			Name:      name,
			Type:      strings.TrimSpace(secret.Type),
			Host:      secret.Host,
			// A configure-created secret belongs to this harness (bound, granted,
			// and deleted with it), so it must not occupy the shared
			// (project,type,host) uniqueness slot: two harnesses may each hold,
			// say, a hostless bearer token.
			UniqueKey:      secretID,
			EncryptedValue: []byte(secret.Value),
		}
		if err := s.store.CreateSecret(ctx, created); err != nil {
			return fmt.Errorf("create configured secret %q: %w", name, err)
		}
		createdSecretIDs = append(createdSecretIDs, created.ID)
		if err := s.store.UpsertHarnessConfigSecretBinding(ctx, &model.HarnessConfigSecretBinding{
			ProjectID:       config.ProjectID,
			HarnessConfigID: config.ID,
			EnvName:         envName,
			SecretID:        created.ID,
		}); err != nil {
			return fmt.Errorf("bind configured secret %q: %w", envName, err)
		}
		// The user just handed this credential to this harness's configure flow,
		// which is the consent the grant records.
		if err := s.store.CreateSecretGrant(ctx, &model.SecretGrant{
			ProjectID: config.ProjectID,
			SecretID:  created.ID,
			Scope:     model.SecretGrantScopeHarnessConfig,
			ScopeKey:  config.ID,
			Host:      secret.Host,
		}); err != nil {
			return fmt.Errorf("grant configured secret %q: %w", envName, err)
		}
	}
	// Reconfiguring replaces the previous generation of configure-created
	// secrets: their bindings were just overwritten above, so keeping the rows
	// would leak one orphaned secret per reconfigure. Secrets the command kept are
	// in createdSecretIDs too, which is what spares them from this sweep.
	replaced := map[string]struct{}{}
	for _, secretID := range createdSecretIDs {
		replaced[secretID] = struct{}{}
	}
	for _, secretID := range config.ConfiguredSecretIDs {
		secretID = strings.TrimSpace(secretID)
		if secretID == "" {
			continue
		}
		if _, ok := replaced[secretID]; ok {
			continue
		}
		if err := s.store.DeleteSecret(ctx, config.ProjectID, secretID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	config.ConfiguredFiles = out.Files
	config.ConfiguredSecretIDs = createdSecretIDs
	return nil
}

// updateConfiguredGrantHost points this harness config's standing grant(s) for a
// secret at a new host, in place, when an update-in-place reconfigure moved the
// secret's host. Only the harness-config-scoped grants this flow created are
// touched.
func (s *Service) updateConfiguredGrantHost(ctx context.Context, config *model.HarnessConfig, secretID, host string) error {
	grants, err := s.store.ListSecretGrants(ctx, config.ProjectID, secretID)
	if err != nil {
		return err
	}
	for i := range grants {
		grant := &grants[i]
		if grant.Scope != model.SecretGrantScopeHarnessConfig || grant.ScopeKey != config.ID {
			continue
		}
		if grant.Host == host {
			continue
		}
		grant.Host = host
		if err := s.store.UpdateSecretGrant(ctx, grant); err != nil {
			return err
		}
	}
	return nil
}

// previousSecretIDsByEnv maps each env name the harness config currently binds to
// a configure-created secret, for resolving an output that asks to keep one.
func (s *Service) previousSecretIDsByEnv(ctx context.Context, config *model.HarnessConfig) (map[string]string, error) {
	configured := make(map[string]struct{}, len(config.ConfiguredSecretIDs))
	for _, secretID := range config.ConfiguredSecretIDs {
		configured[strings.TrimSpace(secretID)] = struct{}{}
	}
	if len(configured) == 0 {
		return nil, nil
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, config.ProjectID, config.ID)
	if err != nil {
		return nil, err
	}
	byEnv := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if _, ok := configured[binding.SecretID]; ok {
			byEnv[binding.EnvName] = binding.SecretID
		}
	}
	return byEnv, nil
}

// previousSentinels maps the configure sandbox's PREV_ sentinels to the secret
// each stands for, so a command that passed one straight through (X=$PREV_X) is
// understood as keeping that secret rather than storing a placeholder as one.
func (s *Service) previousSentinels(ctx context.Context, projectID, sandboxID string) (map[string]string, error) {
	assignments, err := s.store.ListSandboxSecrets(ctx, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	sentinels := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		if strings.HasPrefix(assignment.EnvName, harness.ConfigurePreviousEnvPrefix) {
			sentinels[assignment.Sentinel] = assignment.SecretID
		}
	}
	return sentinels, nil
}

// matchPreviousSentinel reports the secret a configure output value stands for
// when any of its fields is one of the sandbox's PREV_ sentinels.
func matchPreviousSentinel(value json.RawMessage, sentinels map[string]string) string {
	if len(sentinels) == 0 || len(bytes.TrimSpace(value)) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(value, &fields); err != nil {
		return ""
	}
	for _, field := range fields {
		text, ok := field.(string)
		if !ok {
			continue
		}
		if secretID, ok := sentinels[strings.TrimSpace(text)]; ok {
			return secretID
		}
	}
	return ""
}

// DeconfigureHarnessConfig removes exactly what the configure flow created — the
// secrets it made (deleting a secret cascades its bindings and grants) and the
// files it wrote — and marks the harness unconfigured. The image-declared
// baseline (Files, Secrets, RunCommand) is left intact so the harness can simply
// be configured again.
func (s *Service) DeconfigureHarnessConfig(ctx context.Context, projectID, configID string) (*model.HarnessConfig, error) {
	config, err := s.store.GetHarnessConfig(ctx, projectID, configID)
	if err != nil {
		return nil, apiError(err, "harness config not found")
	}
	// Refused for a harness with nothing to configure, because configure is
	// what would undo it and configure refuses that harness too. Turning one
	// off is a door that only opens one way: the config keeps its
	// image-declared baseline but is marked unconfigured, the create path
	// rejects an unconfigured harness, a built-in cannot be deleted, and
	// seeding never revisits Configured. The reserved `shell` built-in is what
	// lands here — born configured because it declares no secrets, and with no
	// configure command to run again.
	if len(config.ConfigCommand) == 0 {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q has nothing to configure, so there is nothing to turn off: its image declares no configure command", config.Slug))
	}
	// The project default must always point at a configured harness, so the
	// default cannot be turned off in place: the client unsets or switches the
	// default first. Deconfiguring it here would leave `run` with no explicit
	// harness resolving to an unconfigured one, which the create path rejects.
	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		return nil, apiError(err, "project not found")
	}
	if project.DefaultHarnessConfigID == config.ID {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("harness %q is the project default; set a different default or unset it before disabling", config.Slug))
	}
	for _, secretID := range config.ConfiguredSecretIDs {
		secretID = strings.TrimSpace(secretID)
		if secretID == "" {
			continue
		}
		if err := s.store.DeleteSecret(ctx, projectID, secretID); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	bindings, err := s.store.ListHarnessConfigSecretBindings(ctx, projectID, configID)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if err := s.store.DeleteHarnessConfigSecretBinding(ctx, projectID, configID, binding.EnvName); err != nil &&
			!errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	config.Configured = false
	config.ConfiguredFiles = nil
	config.ConfiguredSecretIDs = nil
	config.ConfigureError = ""
	if err := s.store.UpdateHarnessConfig(ctx, config); err != nil {
		return nil, err
	}
	return s.store.GetHarnessConfig(ctx, projectID, configID)
}

// Reconcile reaps a configure sandbox whose flow was started but never committed
// (the client crashed, detached, or walked away). It never touches the sandbox
// agent, so it needs no credentials of its own.
func (s *Service) Reconcile(ctx context.Context, configID string) (reconcile.Result, error) {
	config, err := s.store.GetHarnessConfigByID(ctx, configID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}
	sandboxID := strings.TrimSpace(config.ConfigureSandboxID)
	if sandboxID == "" {
		return reconcile.Result{}, nil
	}
	if s.sandboxes == nil || s.dirtier == nil {
		return reconcile.Result{}, errors.New("harness configure requires the sandbox runtime and reconcile engine")
	}
	if deadline := config.UpdatedAt.Add(configureTTL); time.Now().Before(deadline) {
		// Committed configures clear ConfigureSandboxID, so anything still set
		// here is either in progress or abandoned; look again after the TTL.
		// Each in-progress write pushes UpdatedAt out, so the reap only lands
		// once the flow has actually gone quiet for a full TTL.
		return reconcile.RequeueAt(deadline), nil
	}
	slog.InfoContext(ctx, "reaping abandoned configure sandbox",
		"harnessConfigId", config.ID, "sandboxId", sandboxID)
	if err := s.sandboxes.DeleteSandbox(ctx, config.ProjectID, sandboxID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return reconcile.Result{}, err
	}
	config.ConfigureSandboxID = ""
	config.ConfigureError = "configure was never completed and timed out"
	return reconcile.Result{}, s.store.UpdateHarnessConfig(ctx, config)
}

// sandboxAgentClient builds a runner for the sandbox's agent API using the
// caller's credentials.
func (s *Service) sandboxAgentClient(ctx context.Context, projectID, sandboxID string) (*oneShotRunner, func(), error) {
	lease, sandboxModel, err := s.sandboxes.AcquireSandboxHTTPClient(ctx, projectID, sandboxID,
		[]string{poolagentauth.ScopeExecRead, poolagentauth.ScopeExecWrite})
	if err != nil {
		return nil, nil, err
	}
	if lease.Client == nil {
		lease.Release()
		return nil, nil, errors.New("sandbox agent lease has no HTTP client")
	}
	if sandboxModel == nil || strings.TrimSpace(sandboxModel.PoolID) == "" {
		lease.Release()
		return nil, nil, errors.New("sandbox has no pool to reach its agent through")
	}
	return &oneShotRunner{
		httpClient: &http.Client{
			Transport: &leaseAuthTransport{base: lease.Client.Transport, lease: lease},
			Timeout:   lease.Client.Timeout,
		},
		baseURL:   lease.BaseURL,
		projectID: projectID,
		poolID:    strings.TrimSpace(sandboxModel.PoolID),
		sandboxID: sandboxID,
	}, lease.Release, nil
}

// leaseAuthTransport carries both tokens a worker-proxied sandbox-agent request
// needs: the worker's own on Authorization, and the sandbox-agent's on
// X-Discobox-Sandbox-Agent-Authorization for the worker to forward inward.
type leaseAuthTransport struct {
	base  http.RoundTripper
	lease *services.HTTPClientLease
}

func (t *leaseAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	authToken, err := t.lease.AuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(authToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(authToken))
	}
	forwardToken, err := t.lease.ForwardAuthorizationToken(req.Context())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(forwardToken) != "" {
		req.Header.Set("X-Discobox-Sandbox-Agent-Authorization", "Bearer "+strings.TrimSpace(forwardToken))
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// primaryTerminal describes the configure sandbox's primary terminal as the agent
// reports it.
type primaryTerminal struct {
	Status   string `json:"status"`
	ExitCode *int64 `json:"exitCode"`
	Error    string `json:"error"`
}

// settledPrimaryTerminal waits briefly for the primary terminal to record a
// terminal status. A commit normally arrives the instant the attach stream
// closes, which is just before the agent records the exit, so a bare read would
// see "running" for a moment. A caller that genuinely detached mid-flow still
// reads "running" after the wait, and is told so.
func (r *oneShotRunner) settledPrimaryTerminal(ctx context.Context) (primaryTerminal, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, execSettleTimeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		terminal, found, err := r.currentPrimaryTerminal(ctx)
		if err != nil || !found {
			return terminal, found, err
		}
		switch terminal.Status {
		case "exited", "failed", "lost":
			return terminal, true, nil
		}
		select {
		case <-ctx.Done():
			return terminal, true, nil
		case <-ticker.C:
		}
	}
}

// currentPrimaryTerminal returns the sandbox's primary terminal by listing execs,
// without resolving the virtual "primary" id — resolving it would relaunch a
// stopped primary, which here would restart the configure command instead of
// observing that it finished.
func (r *oneShotRunner) currentPrimaryTerminal(ctx context.Context) (primaryTerminal, bool, error) {
	resp, err := r.send(ctx, http.MethodGet, r.url("/execs"), "", nil)
	if err != nil {
		return primaryTerminal{}, false, err
	}
	var list struct {
		Execs []struct {
			primaryTerminal
			Tty       bool      `json:"tty"`
			Primary   bool      `json:"primary"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"execs"`
	}
	if err := json.Unmarshal(resp, &list); err != nil {
		return primaryTerminal{}, false, fmt.Errorf("list execs: %w", err)
	}
	found := false
	var newest primaryTerminal
	var newestAt time.Time
	for _, exec := range list.Execs {
		if !exec.Tty || !exec.Primary {
			continue
		}
		if !found || exec.CreatedAt.After(newestAt) {
			found, newest, newestAt = true, exec.primaryTerminal, exec.CreatedAt
		}
	}
	return newest, found, nil
}

// seedFile writes payload to path inside the sandbox. There is no files API yet,
// so it feeds a `cat` through the exec one-shot form.
func seedFile(ctx context.Context, run *oneShotRunner, path string, payload []byte) error {
	command := []string{"sh", "-c", fmt.Sprintf("mkdir -p %s && cat > %s", shellQuote(pathDir(path)), shellQuote(path))}
	if _, err := run.do(ctx, command, payload); err != nil {
		return fmt.Errorf("seed %s: %w", path, err)
	}
	return nil
}

// readConfigureOutput reads the file the configure command wrote.
func readConfigureOutput(ctx context.Context, run *oneShotRunner) (*configureOutput, error) {
	stdout, err := run.do(ctx, []string{"cat", harness.ConfigureOutputPath}, nil)
	if err != nil {
		return nil, fmt.Errorf("configure flow did not write %s: %w", harness.ConfigureOutputPath, err)
	}
	var out configureOutput
	if err := json.Unmarshal(stdout, &out); err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", harness.ConfigureOutputPath, err)
	}
	return &out, nil
}

// oneShotRunner runs short-lived commands in a sandbox through the exec one-shot
// form: POST the attach with the command's stdin as the body and read its output
// back as the response.
//
// It addresses the worker, not the sandbox directly. A sandbox-agent lease points
// at the worker that hosts the sandbox, and the worker passes exec routes through
// to the agent under its own scheme — so requests carry two tokens: the worker's
// on Authorization, and the sandbox-agent's on X-Discobox-Sandbox-Agent-Authorization
// for the worker to forward inward. The generated sandbox client cannot express
// that route shape, so these three calls are made directly.
type oneShotRunner struct {
	httpClient *http.Client
	baseURL    string
	projectID  string
	poolID     string
	sandboxID  string
}

func (r *oneShotRunner) url(suffix string) string {
	return fmt.Sprintf("%s/api/project/%s/pool/%s/sandboxes/%s%s",
		strings.TrimRight(r.baseURL, "/"),
		neturl.PathEscape(r.projectID), neturl.PathEscape(r.poolID), neturl.PathEscape(r.sandboxID), suffix)
}

// do runs command with stdin as its input and returns its output. A non-zero exit
// is an error: the response body carries the output, and the exec record is
// authoritative for the status.
func (r *oneShotRunner) do(ctx context.Context, command []string, stdin []byte) ([]byte, error) {
	execID, err := r.createExec(ctx, command)
	if err != nil {
		return nil, err
	}
	out, err := r.attachOneShot(ctx, execID, stdin)
	if err != nil {
		return nil, err
	}
	code, err := r.execExitCode(ctx, execID)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("exited with code %d: %s", code, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func (r *oneShotRunner) createExec(ctx context.Context, command []string) (string, error) {
	body, err := json.Marshal(map[string]any{"command": command})
	if err != nil {
		return "", err
	}
	resp, err := r.send(ctx, http.MethodPost, r.url("/execs"), "application/json", body)
	if err != nil {
		return "", err
	}
	var created struct {
		Exec struct {
			ID string `json:"id"`
		} `json:"exec"`
	}
	if err := json.Unmarshal(resp, &created); err != nil {
		return "", fmt.Errorf("create exec: %w", err)
	}
	if created.Exec.ID == "" {
		return "", errors.New("create exec: response carried no exec id")
	}
	return created.Exec.ID, nil
}

// attachOneShot POSTs the exec's attach endpoint, which runs it to completion with
// the request body as stdin and returns its output.
func (r *oneShotRunner) attachOneShot(ctx context.Context, execID string, stdin []byte) ([]byte, error) {
	return r.send(ctx, http.MethodPost, r.url("/execs/"+neturl.PathEscape(execID)+"/attach"),
		"application/octet-stream", stdin)
}

// execExitCode reads an exec's recorded exit status, waiting briefly for it to
// settle. The one-shot attach returns as soon as the process's output stream
// closes, but the agent records the exit status separately, so the record can
// still read "running" for a moment afterward.
func (r *oneShotRunner) execExitCode(ctx context.Context, execID string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, execSettleTimeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		resp, err := r.send(ctx, http.MethodGet, r.url("/execs/"+neturl.PathEscape(execID)), "", nil)
		if err != nil {
			return 0, err
		}
		var exec struct {
			Status   string `json:"status"`
			ExitCode *int64 `json:"exitCode"`
		}
		if err := json.Unmarshal(resp, &exec); err != nil {
			return 0, fmt.Errorf("read exec status: %w", err)
		}
		if exec.ExitCode != nil {
			return *exec.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("exec did not report an exit code (status %s)", exec.Status)
		case <-ticker.C:
		}
	}
}

func (r *oneShotRunner) send(ctx context.Context, method, url, contentType string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("%s %s: %s: %s", method, url, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func derefExitCode(code *int64) int64 {
	if code == nil {
		return -1
	}
	return *code
}

func pathDir(path string) string {
	if idx := strings.LastIndex(path, "/"); idx > 0 {
		return path[:idx]
	}
	return "/"
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func configureSandboxName() (string, error) {
	newID, err := id.New(id.PrefixSandbox)
	if err != nil {
		return "", err
	}
	return "configure-" + id.RandomPart(newID)[:8], nil
}
