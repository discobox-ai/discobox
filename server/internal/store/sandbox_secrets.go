package store

import (
	"context"
	"errors"

	"gorm.io/gorm"

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

// ListLiveAgentCredentials returns a sandbox's agent-requested bindings joined
// with the grants that authorize them, dropping any whose grant has lapsed. It
// is what the agent credentials protocol's list operation reports.
func (s *Store) ListLiveAgentCredentials(ctx context.Context, projectID, sandboxID string) ([]AgentCredential, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var assignments []model.SandboxSecret
	if err := read.Where("project_id = ? AND sandbox_id = ? AND agent_requested = ?", projectID, sandboxID, true).
		Order("env_name ASC").Find(&assignments).Error; err != nil {
		return nil, err
	}
	if len(assignments) == 0 {
		return nil, nil
	}
	out := make([]AgentCredential, 0, len(assignments))
	for i := range assignments {
		assignment := assignments[i]
		// The grant is matched at the sandbox scope only. This flow never
		// produces a broader one, and a wider standing grant that happens to
		// cover the same secret is not an approval of these uses.
		grant, err := s.FindLiveAgentGrant(ctx, projectID, assignment.SecretID, sandboxID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // the approval behind this binding has lapsed
			}
			return nil, err
		}
		secret, err := s.GetSecret(ctx, projectID, assignment.SecretID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue // secret deleted out from under the binding
			}
			return nil, err
		}
		out = append(out, AgentCredential{
			Assignment: assignment,
			Grant:      *grant,
			Name:       secret.Name,
			Format:     secret.Format,
		})
	}
	return out, nil
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
