package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/secretroute"
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
		// A binding already naming this secret is the common case, and it
		// writes nothing: it is read here rather than queued behind every other
		// write for BindAgentSecret's lock. Only a missing or contested binding
		// needs that lock, and reading without it is safe because this path
		// then changes nothing.
		binding, err := s.FindAgentSandboxSecret(ctx, projectID, sandboxID, envName)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		if binding == nil || binding.SecretID != secret.ID {
			binding, err = s.BindAgentSecret(ctx, projectID, sandboxID, envName, secret)
		}
		if errors.Is(err, ErrAgentVariableHeld) {
			// Another secret's live grant holds the variable, as when two
			// grants of one scope name it. This grant is passed over as a
			// wider one is above, rather than failing every credential the
			// discobox has over one contested variable.
			continue
		}
		if err != nil {
			return nil, err
		}
		claimed[envName] = grant.SecretID
		out = append(out, AgentCredential{
			Assignment:   *binding,
			Grant:        grant,
			Name:         secret.Name,
			Format:       s.SentinelFormat(ctx, secret),
			SecretRoutes: secret.Routes,
		})
	}
	return out, nil
}

// BindAgentSecret is the discobox's stable binding of one variable to one
// secret: the sentinel the pool agent translates a use's ephemeral one back to.
// The row is marked AgentRequested, so it is never injected into the sandbox
// (ADR 0031 §4). Every path that binds an existing discobox goes through here
// — approving an agent's request, a grant made ahead of the asking, and the
// lazy binding of a wider grant in ListLiveAgentCredentials. A discobox being
// created has nothing bound yet, so its bindings are built by NewAgentBinding
// and stored with it.
//
// A variable already bound to this secret keeps its binding, so a sentinel an
// earlier activation was minted from stays resolvable. A variable bound to
// another secret is rebound only when nothing still delivers that binding: no
// live grant with uses, at any scope covering the discobox, names its secret
// for this variable. Such a binding gives the discobox nothing — a resolve
// hands a value out only under a live grant — so the refusal would only lock
// the variable to a credential nobody may use. While a grant does deliver it,
// rebinding would leave a live activation resolving to a different secret, the
// silent swap this flow exists to prevent, and it is refused with
// ErrAgentVariableHeld, naming the grant to revoke. A rebind mints a
// fresh sentinel, so one minted under the old secret never resolves to the new
// one.
//
// A variable with no binding has no row to lock, so two first binds of it can
// race to create one, and on a database that does not serialize writers the
// loser hits the unique index. It binds again once, finding the winner's row
// and deciding by the rule above, and a second collision is a 409 rather than
// a driver's constraint text.
func (s *Store) BindAgentSecret(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) (*model.SandboxSecret, error) {
	binding, err := s.bindAgentSecretOnce(ctx, projectID, sandboxID, envName, secret)
	if !isUniqueViolation(err) {
		return binding, err
	}
	binding, err = s.bindAgentSecretOnce(ctx, projectID, sandboxID, envName, secret)
	if isUniqueViolation(err) {
		return nil, apperrors.NewStatusError(http.StatusConflict,
			fmt.Sprintf("the agent credential in %s changed concurrently; try again", envName))
	}
	return binding, err
}

func (s *Store) bindAgentSecretOnce(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) (*model.SandboxSecret, error) {
	var out *model.SandboxSecret
	// One transaction, holding the binding's row while the grants are read: an
	// approval of the bound secret commits its grant either before this reads
	// the grants, and holds the variable, or after the rebind, and finds the
	// variable gone to another secret.
	err := s.Transaction(ctx, func(tx *Store, txDB *gorm.DB) error {
		var existing model.SandboxSecret
		err := txDB.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("project_id = ? AND sandbox_id = ? AND env_name = ? AND agent_requested = ?",
				projectID, sandboxID, envName, true).
			First(&existing).Error
		found := err == nil
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if found && existing.SecretID == secret.ID {
			out = &existing
			return nil
		}
		binding, err := tx.NewAgentBinding(ctx, projectID, sandboxID, envName, secret)
		if err != nil {
			return err
		}
		if !found {
			if err := tx.CreateSandboxSecret(ctx, binding); err != nil {
				return err
			}
			out = binding
			return nil
		}
		holder, err := tx.agentBindingHolder(ctx, &existing)
		if err != nil {
			return err
		}
		if holder != nil {
			return apperrors.StatusError{
				Status: http.StatusConflict,
				Message: fmt.Sprintf("discobox already has an agent credential in %s from another secret, delivered by %s-scoped grant %s; revoke that grant to bind %s to this one",
					envName, holder.Scope, holder.ID, envName),
				Cause: ErrAgentVariableHeld,
			}
		}
		binding.ID = existing.ID
		if err := tx.UpdateSandboxSecret(ctx, binding); err != nil {
			return err
		}
		out = binding
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isUniqueViolation reports whether err is a unique index refusing a write, in
// either driver's wording: SQLite's "UNIQUE constraint failed" and Postgres's
// "violates unique constraint".
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

// NewAgentBinding is an agent credential's binding, built but not stored: a
// freshly minted stable sentinel for one variable and secret on one discobox.
func (s *Store) NewAgentBinding(ctx context.Context, projectID, sandboxID, envName string, secret *model.Secret) (*model.SandboxSecret, error) {
	format := s.SentinelFormat(ctx, secret)
	sentinel, err := secretformat.MintSentinel(format)
	if err != nil {
		return nil, err
	}
	return &model.SandboxSecret{
		ProjectID:      projectID,
		SandboxID:      sandboxID,
		SecretID:       secret.ID,
		EnvName:        envName,
		Sentinel:       sentinel,
		Format:         format,
		AgentRequested: true,
	}, nil
}

// agentBindingHolder is the live grant that still delivers a binding, or nil:
// one with uses, covering the discobox at any scope, naming the binding's
// secret for its variable. A grant from before the variable was recorded on it
// delivers whichever binding its secret has (ListLiveAgentCredentials).
//
// Only a use grant counts. A standing grant of the same secret authorizes the
// sentinel the sandbox is provisioned with, never this one: the binding's
// sentinel never reaches the sandbox, and the pool agent mints an activation of
// it only for a credential ListLiveAgentCredentials lists.
func (s *Store) agentBindingHolder(ctx context.Context, binding *model.SandboxSecret) (*model.SecretGrant, error) {
	sandbox, err := s.GetSandbox(ctx, binding.ProjectID, binding.SandboxID)
	if err != nil {
		return nil, err
	}
	grants, err := s.ListLiveAgentGrants(ctx, binding.ProjectID, SandboxGrantScopes(sandbox))
	if err != nil {
		return nil, err
	}
	for i := range grants {
		if grants[i].SecretID != binding.SecretID {
			continue
		}
		if name := strings.TrimSpace(grants[i].EnvName); name == "" || name == binding.EnvName {
			return &grants[i], nil
		}
	}
	return nil, nil
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
	// SecretRoutes are where the secret may be sent (ADR 26-10-02-924): a
	// request an activation of this credential is sent in must fall on one,
	// as well as within the grant's use.
	SecretRoutes []model.SecretRoute
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
// writer: a harness-config rebind repoints an assignment at the secret its
// binding now names, re-minting the sentinel when the new secret's format
// differs, and BindAgentSecret repoints an agent binding no grant still delivers,
// always under a fresh sentinel.
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

// SecretSentinelRoutes maps each of a discobox's injected sentinels to the
// routes of the secret it stands for, when that secret carries routes (ADR
// 26-10-02-924): every request the sentinel is sent in must fall on one.
// assignments are the discobox's injected secrets. A judge discobox's own are
// held to none, since its harness's requests would wait on its own verdict.
func (s *Store) SecretSentinelRoutes(ctx context.Context, sandbox *model.Sandbox, assignments []model.SandboxSecret) (map[string][]secretroute.Route, error) {
	if sandbox.HarnessMode == sandboxconfig.HarnessModeJudge {
		return nil, nil
	}
	var out map[string][]secretroute.Route
	for _, assignment := range assignments {
		secret, err := s.GetSecret(ctx, sandbox.ProjectID, assignment.SecretID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, err
		}
		routes := secret.ProxyRoutes()
		if len(routes) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]secretroute.Route{}
		}
		out[assignment.Sentinel] = routes
	}
	return out, nil
}

// ListSandboxIDsHoldingSecret returns the discoboxes with a sentinel injected
// for a secret, so a change to where the secret may be sent reaches them.
func (s *Store) ListSandboxIDsHoldingSecret(ctx context.Context, projectID, secretID string) ([]string, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	err = read.Model(&model.SandboxSecret{}).
		Where("project_id = ? AND secret_id = ? AND agent_requested = ?", projectID, secretID, false).
		Distinct("sandbox_id").Order("sandbox_id ASC").
		Pluck("sandbox_id", &out).Error
	return out, err
}
