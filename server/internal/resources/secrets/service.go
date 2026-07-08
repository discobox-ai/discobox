package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	apigen "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/auth"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secretformat"
	"github.com/obot-platform/discobox/server/internal/services"
	"github.com/obot-platform/discobox/server/internal/store"
)

const defaultGrantTTLSeconds = 3600

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListSecrets(ctx, projectID)
}

func (s *Service) CreateSecret(ctx context.Context, projectID string, input services.CreateSecretBody) (*model.Secret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret name is required")
	}
	secretType := string(input.Type)
	if !validSecretType(secretType) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret type")
	}
	valueBytes, err := marshalSecretValue(input.Value)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
	}
	ttl := int64(defaultGrantTTLSeconds)
	if v, ok := input.DefaultGrantTTLSeconds.Get(); ok && v > 0 {
		ttl = v
	}
	host := ""
	if v, ok := input.Host.Get(); ok {
		host = strings.TrimSpace(v)
	}
	format := ""
	if secretType == model.SecretTypeBearer {
		if token := strings.TrimSpace(input.Value.Token.Or("")); token != "" {
			inferredFormat, inferredHost := secretformat.Describe(token)
			format = inferredFormat
			if host == "" {
				host = inferredHost
			}
		}
	}
	sec := &model.Secret{
		ProjectID:       projectID,
		Name:            name,
		Type:            secretType,
		Host:            host,
		Format:          format,
		DefaultGrantTTL: ttl,
		EncryptedValue:  valueBytes,
	}
	if err := s.store.CreateSecret(ctx, sec); err != nil {
		return nil, err
	}
	return s.store.GetSecret(ctx, projectID, sec.ID)
}

func (s *Service) GetSecret(ctx context.Context, projectID, secretID string) (*model.Secret, error) {
	sec, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	return sec, nil
}

func (s *Service) UpdateSecret(ctx context.Context, projectID, secretID string, input services.UpdateSecretBody) (*model.Secret, error) {
	sec, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	if nameVal, ok := input.Name.Get(); ok {
		name := strings.TrimSpace(nameVal)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret name is required")
		}
		sec.Name = name
	}
	if hostVal, ok := input.Host.Get(); ok {
		sec.Host = strings.TrimSpace(hostVal)
	}
	if ttl, ok := input.DefaultGrantTTLSeconds.Get(); ok && ttl > 0 {
		sec.DefaultGrantTTL = ttl
	}
	if valueVal, ok := input.Value.Get(); ok {
		valueBytes, err := marshalSecretValue(valueVal)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
		}
		sec.EncryptedValue = valueBytes
		if sec.Type == model.SecretTypeBearer {
			if token := strings.TrimSpace(valueVal.Token.Or("")); token != "" {
				sec.Format, _ = secretformat.Describe(token)
			}
		}
	}
	if err := s.store.UpdateSecret(ctx, sec); err != nil {
		return nil, err
	}
	return s.store.GetSecret(ctx, projectID, sec.ID)
}

func (s *Service) DeleteSecret(ctx context.Context, projectID, secretID string) error {
	if err := s.store.DeleteSecret(ctx, projectID, secretID); err != nil {
		return apiError(err, "secret not found")
	}
	return nil
}

func (s *Service) ListSecretRequests(ctx context.Context, projectID, status string) ([]model.SecretRequest, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	return s.store.ListSecretRequests(ctx, projectID, status)
}

func (s *Service) CreateSecretRequest(ctx context.Context, projectID string, input services.CreateSecretRequestBody) (*model.SecretRequest, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	secretType := string(input.Type)
	if !validSecretType(secretType) {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret type")
	}
	host := ""
	if v, ok := input.Host.Get(); ok {
		host = strings.TrimSpace(v)
	}

	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusUnauthorized, "authentication required")
	}
	requestedBy := principal.UserID
	if requestedBy == "" {
		requestedBy = principal.WorkerID
	}
	if requestedBy == "" {
		return nil, apperrors.NewStatusError(http.StatusUnauthorized, "could not determine requesting principal")
	}

	req := &model.SecretRequest{
		ProjectID:   projectID,
		RequestedBy: requestedBy,
		Type:        secretType,
		Host:        host,
		Status:      model.SecretRequestStatusPending,
	}

	// A non-sandbox request is satisfied immediately only by a project-wide grant
	// on a matching secret; otherwise it waits for approval.
	matched, err := s.store.MatchSecret(ctx, projectID, secretType, host)
	if err != nil && !isAdvisoryMatchError(err) {
		return nil, err
	}
	if err == nil {
		grant, gerr := s.store.FindLiveGrant(ctx, projectID, matched.ID, host, []store.GrantScope{{Scope: model.SecretGrantScopeProject, ScopeKey: projectID}})
		if gerr != nil && !errors.Is(gerr, store.ErrNotFound) {
			return nil, gerr
		}
		if grant != nil {
			req.SecretID = matched.ID
			req.Status = model.SecretRequestStatusApproved
			req.GrantID = grant.ID
		}
	}

	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

func (s *Service) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apiError(err, "secret request not found")
	}
	return req, nil
}

// ApproveSecretRequest approves a pending request by minting a SecretGrant at the
// chosen scope and linking it to the request. The grant is the durable
// authorization future resolutions match against; the request is only marked
// approved for audit.
func (s *Service) ApproveSecretRequest(ctx context.Context, projectID, requestID string, input services.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apiError(err, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusPending {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}

	secretID := strings.TrimSpace(input.SecretId)
	if secretID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required")
	}
	secret, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}

	scope := strings.TrimSpace(string(input.Scope.Or("")))
	if scope == "" {
		// Default to the narrowest scope the request can support.
		if req.SandboxID != "" {
			scope = model.SecretGrantScopeSandbox
		} else {
			scope = model.SecretGrantScopeProject
		}
	}
	scopeKey, err := s.grantScopeKey(ctx, projectID, req.SandboxID, scope)
	if err != nil {
		return nil, err
	}

	ttl := secret.DefaultGrantTTL
	if v, ok := input.GrantTTLSeconds.Get(); ok && v > 0 {
		ttl = v
	}
	grant, err := s.mintGrant(ctx, projectID, secret.ID, scope, scopeKey, req.Host, ttl)
	if err != nil {
		return nil, err
	}

	req.SecretID = secret.ID
	req.Status = model.SecretRequestStatusApproved
	req.GrantID = grant.ID
	if err := s.store.UpdateSecretRequestIfPending(ctx, req); err != nil {
		// Avoid leaving a live authorization behind if the request was denied or
		// approved concurrently.
		_ = s.store.DeleteSecretGrant(ctx, projectID, grant.ID)
		if errors.Is(err, store.ErrGenerationConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
		}
		return nil, err
	}
	return req, nil
}

func (s *Service) DenySecretRequest(ctx context.Context, projectID, requestID string) error {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return apiError(err, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusPending {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}
	req.Status = model.SecretRequestStatusDenied
	if err := s.store.UpdateSecretRequestIfPending(ctx, req); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
		}
		return err
	}
	return nil
}

// ResolveSandboxSecret resolves a sentinel injected into a sandbox. It maps the
// sentinel to its assignment and looks for a live grant covering the sandbox at
// any scope (its own ID, its agent config, or the project). A match returns the
// decrypted value; otherwise a single pending request is created (or reused) and
// the proxy leaves the sentinel in place until a grant exists.
func (s *Service) ResolveSandboxSecret(ctx context.Context, workerID, sandboxID, sentinel, host string) (*model.SandboxSecretResolution, error) {
	assignment, err := s.store.GetSandboxSecretBySentinel(ctx, sandboxID, sentinel)
	if err != nil {
		return nil, apiError(err, "sandbox secret not found")
	}
	// The calling worker must own the sandbox the sentinel belongs to.
	sandbox, err := s.store.GetSandbox(ctx, assignment.ProjectID, assignment.SandboxID)
	if err != nil {
		return nil, apiError(err, "sandbox not found")
	}
	if sandbox.WorkerID == nil || strings.TrimSpace(*sandbox.WorkerID) != strings.TrimSpace(workerID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox secret not found")
	}
	secret, err := s.store.GetSecret(ctx, assignment.ProjectID, assignment.SecretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	host = strings.TrimSpace(host)

	scopes := []store.GrantScope{
		{Scope: model.SecretGrantScopeSandbox, ScopeKey: sandbox.ID},
		{Scope: model.SecretGrantScopeProject, ScopeKey: assignment.ProjectID},
	}
	if sandbox.AgentConfigID != nil && strings.TrimSpace(*sandbox.AgentConfigID) != "" {
		scopes = append(scopes, store.GrantScope{Scope: model.SecretGrantScopeAgentConfig, ScopeKey: strings.TrimSpace(*sandbox.AgentConfigID)})
	}
	grant, err := s.store.FindLiveGrant(ctx, assignment.ProjectID, assignment.SecretID, host, scopes)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if grant != nil {
		val, err := s.store.OpenSecretValue(ctx, secret)
		if err != nil {
			return nil, fmt.Errorf("decrypt secret: %w", err)
		}
		return &model.SandboxSecretResolution{Status: model.SecretRequestStatusApproved, Value: val, ExpiresAt: grant.ExpiresAt}, nil
	}

	// No grant: ensure exactly one pending request exists for this sandbox+secret+host.
	requestedBy := "sandbox:" + sandboxID
	existing, err := s.store.FindPendingSecretRequest(ctx, assignment.ProjectID, assignment.SecretID, host, requestedBy)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if existing == nil {
		req := &model.SecretRequest{
			ProjectID:   assignment.ProjectID,
			RequestedBy: requestedBy,
			SandboxID:   assignment.SandboxID,
			Type:        secret.Type,
			Host:        host,
			SecretID:    secret.ID,
			Status:      model.SecretRequestStatusPending,
		}
		if err := s.store.CreateSecretRequest(ctx, req); err != nil {
			return nil, err
		}
	}
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusPending}, nil
}

// ListSecretGrants returns a project's grants, optionally filtered to one secret.
func (s *Service) ListSecretGrants(ctx context.Context, projectID, secretID string) ([]model.SecretGrant, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	secretID = strings.TrimSpace(secretID)
	if secretID != "" {
		if _, err := s.store.GetSecret(ctx, projectID, secretID); err != nil {
			return nil, apiError(err, "secret not found")
		}
	}
	return s.store.ListSecretGrants(ctx, projectID, secretID)
}

// CreateSecretGrant mints a standing grant directly (pre-approval), without a
// prior request.
func (s *Service) CreateSecretGrant(ctx context.Context, projectID string, input services.CreateSecretGrantBody) (*model.SecretGrant, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	secretID := strings.TrimSpace(input.SecretId)
	if secretID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required")
	}
	secret, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	scope := strings.TrimSpace(string(input.Scope))
	scopeKey := strings.TrimSpace(input.ScopeKey.Or(""))
	if scope == model.SecretGrantScopeProject && scopeKey == "" {
		scopeKey = projectID
	}
	if err := validateGrantScope(scope, scopeKey); err != nil {
		return nil, err
	}
	host := strings.TrimSpace(input.Host.Or(secret.Host))
	// Default to the secret's TTL; an explicit value (including 0 = never expires) wins.
	ttl := secret.DefaultGrantTTL
	if v, ok := input.GrantTTLSeconds.Get(); ok {
		ttl = v
	}
	return s.mintGrant(ctx, projectID, secret.ID, scope, scopeKey, host, ttl)
}

// RevokeSecretGrant deletes a standing grant.
func (s *Service) RevokeSecretGrant(ctx context.Context, projectID, grantID string) error {
	if err := s.store.DeleteSecretGrant(ctx, projectID, grantID); err != nil {
		return apiError(err, "secret grant not found")
	}
	return nil
}

// grantScopeKey resolves the identifier a grant scope binds to for a request.
func (s *Service) grantScopeKey(ctx context.Context, projectID, sandboxID, scope string) (string, error) {
	switch scope {
	case model.SecretGrantScopeProject:
		return projectID, nil
	case model.SecretGrantScopeSandbox:
		if sandboxID == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "sandbox scope requires a sandbox-originated request")
		}
		return sandboxID, nil
	case model.SecretGrantScopeAgentConfig:
		if sandboxID == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "agentConfig scope requires a sandbox-originated request")
		}
		sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return "", apiError(err, "sandbox not found")
		}
		if sandbox.AgentConfigID == nil || strings.TrimSpace(*sandbox.AgentConfigID) == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "sandbox has no agent config to scope the grant to")
		}
		return strings.TrimSpace(*sandbox.AgentConfigID), nil
	default:
		return "", apperrors.NewStatusError(http.StatusBadRequest, "grant scope must be sandbox, agentConfig, or project")
	}
}

func (s *Service) mintGrant(ctx context.Context, projectID, secretID, scope, scopeKey, host string, ttlSeconds int64) (*model.SecretGrant, error) {
	principal, _ := auth.PrincipalFromContext(ctx)
	grantedBy := principal.UserID
	if grantedBy == "" {
		grantedBy = principal.WorkerID
	}
	grant := &model.SecretGrant{
		ProjectID: projectID,
		SecretID:  secretID,
		Scope:     scope,
		ScopeKey:  scopeKey,
		Host:      host,
		GrantedBy: grantedBy,
	}
	if ttlSeconds > 0 {
		exp := time.Now().UTC().Add(time.Duration(ttlSeconds) * time.Second)
		grant.ExpiresAt = &exp
	}
	if err := s.store.CreateSecretGrant(ctx, grant); err != nil {
		return nil, err
	}
	return grant, nil
}

func validateGrantScope(scope, scopeKey string) error {
	switch scope {
	case model.SecretGrantScopeSandbox, model.SecretGrantScopeAgentConfig, model.SecretGrantScopeProject:
	default:
		return apperrors.NewStatusError(http.StatusBadRequest, "grant scope must be sandbox, agentConfig, or project")
	}
	if scopeKey == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "grant scopeKey is required")
	}
	return nil
}

// marshalSecretValue converts a generated SecretValue to JSON plaintext for encryption.
func marshalSecretValue(val apigen.SecretValue) ([]byte, error) {
	mv := model.SecretValue{
		Username:   val.Username.Or(""),
		Password:   val.Password.Or(""),
		PrivateKey: val.PrivateKey.Or(""),
		Passphrase: val.Passphrase.Or(""),
		Token:      val.Token.Or(""),
	}
	//nolint:gosec // Secret values are intentionally marshaled before store encryption.
	return json.Marshal(mv)
}

func validSecretType(t string) bool {
	return t == model.SecretTypeGit || t == model.SecretTypeSSH || t == model.SecretTypeBearer
}

func apiError(err error, notFoundMessage string) error {
	if errors.Is(err, store.ErrNotFound) {
		return apperrors.NewStatusError(http.StatusNotFound, notFoundMessage)
	}
	return err
}

func isAdvisoryMatchError(err error) bool {
	var statusErr interface{ StatusCode() int }
	return errors.As(err, &statusErr) && (statusErr.StatusCode() == http.StatusNotFound || statusErr.StatusCode() == http.StatusConflict)
}
