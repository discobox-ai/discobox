package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// CreateSandboxSecret persists a sandbox secret assignment.
func (s *Store) CreateSandboxSecret(ctx context.Context, assignment *model.SandboxSecret) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(assignment).Error
}

// ListSandboxSecrets returns every secret assignment for a sandbox, including
// agent-requested bindings. Callers that are about to hand sentinels to the
// sandbox or its proxy want ListInjectedSandboxSecrets instead.
func (s *Store) ListSandboxSecrets(ctx context.Context, projectID, sandboxID string) ([]model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ?", projectID, sandboxID).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// ListInjectedSandboxSecrets returns only the assignments whose sentinel is
// provisioned into the sandbox and registered with the proxy.
//
// Agent-requested bindings are excluded here rather than at each call site: a
// binding created by approving an agent credential request must never reach the
// sandbox environment, secrets.json, or the proxy's sentinel set, and a filter
// every caller has to remember is a filter someone eventually forgets
// (ADR 0031 §4).
func (s *Store) ListInjectedSandboxSecrets(ctx context.Context, projectID, sandboxID string) ([]model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ? AND agent_requested = ?", projectID, sandboxID, false).
		Order("env_name ASC").Find(&out).Error
	return out, err
}

// ListLiveAgentCredentials is what a discobox's agent may ask for: every live
// grant that carries uses and covers this discobox, with the binding each one
// resolves through.
//
// It starts from the grants rather than from the bindings, which is what lets a
// grant be wider than one discobox. A grant on a harness config or a project
// has no binding when it is written — the discoboxes it covers may not exist
// yet — so the binding is minted here, the first time that discobox's agent
// asks. Minting is a write on a read path, and it is the cheaper of the two
// honest options: the other is a reconciler chasing every discobox against
// every grant, including ones nobody has created.
func (s *Store) ListLiveAgentCredentials(ctx context.Context, projectID, sandboxID string, scopes []GrantScope) ([]AgentCredential, error) {
	grants, err := s.ListLiveAgentGrants(ctx, projectID, scopes)
	if err != nil {
		return nil, err
	}
	out := make([]AgentCredential, 0, len(grants))
	claimed := map[string]string{} // env var -> secret that took it
	for i := range grants {
		grant := grants[i]
		envName := strings.TrimSpace(grant.EnvName)
		if envName == "" {
			// A grant from before the variable was recorded on it. Its binding
			// carries the name, so it is found by secret rather than by
			// variable.
			binding, err := s.findAgentBindingBySecret(ctx, projectID, sandboxID, grant.SecretID)
			if err != nil || binding == nil {
				continue
			}
			envName = binding.EnvName
		}
		// Narrowest first, so a grant written about this discobox keeps its
		// variable and a wider one naming the same variable is passed over
		// rather than silently swapping the credential underneath it.
		if took, taken := claimed[envName]; taken && took != grant.SecretID {
			continue
		}
		secret, err := s.GetSecret(ctx, projectID, grant.SecretID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // the secret went out from under the grant
			}
			return nil, err
		}
		binding, err := s.EnsureAgentBinding(ctx, projectID, sandboxID, envName, secret)
		if err != nil {
			return nil, err
		}
		claimed[envName] = grant.SecretID
		out = append(out, AgentCredential{
			Assignment: *binding,
			Grant:      grant,
			Name:       secret.Name,
			Format:     secret.Format,
		})
	}
	return out, nil
}

// EnsureAgentBinding is the discobox's stable sentinel for one credential,
// minted if this is the first time its agent has asked. The row is never
// injected into the sandbox (ADR 0031 §4); it exists so the pool agent's
// ephemeral sentinels have something to translate back to.
func (s *Store) EnsureAgentBinding(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) (*model.SandboxSecret, error) {
	existing, err := s.FindAgentSandboxSecret(ctx, projectID, sandboxID, envName)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.SecretID == secret.ID {
		return existing, nil
	}
	if existing != nil {
		// One variable, one credential. Rebinding it would leave a live
		// activation resolving to a different secret.
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("discobox already has an agent credential in %s from another secret", envName))
	}
	sentinel, err := secretformat.MintSentinel(secret.Format)
	if err != nil {
		return nil, err
	}
	binding := &model.SandboxSecret{
		ProjectID:      projectID,
		SandboxID:      sandboxID,
		SecretID:       secret.ID,
		EnvName:        envName,
		Sentinel:       sentinel,
		AgentRequested: true,
	}
	if err := s.CreateSandboxSecret(ctx, binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// findAgentBindingBySecret is the binding for one secret on one discobox,
// whatever variable it was bound to.
func (s *Store) findAgentBindingBySecret(ctx context.Context, projectID, sandboxID, secretID string) (*model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ? AND secret_id = ? AND agent_requested = ?",
		projectID, sandboxID, secretID, true).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// AgentCredential is one agent-requested binding and the live grant authorizing
// it. Sentinel and Format never leave the trusted side: the pool agent needs
// them to mint an ephemeral sentinel that byte-mimics the real key and to
// translate it back, and the sandbox sees neither.
type AgentCredential struct {
	Assignment model.SandboxSecret
	Grant      model.SecretGrant
	Name       string
	Format     string
}

// FindAgentSandboxSecret returns a sandbox's agent-requested binding for one
// environment variable, or ErrNotFound.
func (s *Store) FindAgentSandboxSecret(ctx context.Context, projectID, sandboxID, envName string) (*model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SandboxSecret
	err = read.Where("project_id = ? AND sandbox_id = ? AND env_name = ? AND agent_requested = ?",
		projectID, sandboxID, envName, true).First(&out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetSandboxSecretBySentinel returns the assignment for a sandbox and sentinel.
func (s *Store) GetSandboxSecretBySentinel(ctx context.Context, sandboxID, sentinel string) (*model.SandboxSecret, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out model.SandboxSecret
	if err := read.Where("sandbox_id = ? AND sentinel = ?", sandboxID, sentinel).First(&out).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &out, nil
}

// DeleteSandboxSecrets removes all secret assignments for a sandbox.
func (s *Store) DeleteSandboxSecrets(ctx context.Context, sandboxID string) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Where("sandbox_id = ?", sandboxID).Delete(&model.SandboxSecret{}).Error
}

// UpdateSandboxSecret persists a changed assignment. Rebinding is the only
// writer: it repoints an assignment at the secret its binding now names, and
// re-mints the sentinel when the new secret's format differs.
func (s *Store) UpdateSandboxSecret(ctx context.Context, assignment *model.SandboxSecret) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Model(&model.SandboxSecret{}).
		Where("id = ?", assignment.ID).
		Updates(map[string]any{
			"secret_id": assignment.SecretID,
			"sentinel":  assignment.Sentinel,
			"format":    assignment.Format,
		}).Error
}

// deleteSandboxSecretsBySecret removes every assignment naming a secret. It runs
// inside DeleteSecret's transaction: an assignment whose secret is gone is a
// sentinel the proxy still swaps on but can never resolve, which reaches the
// harness as an unexplained 401 rather than a missing credential.
func (s *Store) deleteSandboxSecretsBySecret(tx *gorm.DB, secretID string) error {
	return tx.Where("secret_id = ?", secretID).Delete(&model.SandboxSecret{}).Error
}

// ListSandboxIDsForHarnessConfig returns the non-archived sandboxes running a
// harness config, which are the sandboxes a binding change has to reach.
func (s *Store) ListSandboxIDsForHarnessConfig(ctx context.Context, projectID, harnessConfigID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	err = read.Model(&model.Sandbox{}).
		Where("project_id = ? AND harness_config_id = ?", projectID, harnessConfigID).
		Where("state NOT IN ?", []string{model.SandboxStateArchived, model.SandboxStateDeleted}).
		Order("id ASC").
		Pluck("id", &out).Error
	return out, err
}
