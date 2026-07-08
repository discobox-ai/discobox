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
		AutoApprove:     input.AutoApprove.Or(false),
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
	if autoApprove, ok := input.AutoApprove.Get(); ok {
		sec.AutoApprove = autoApprove
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

	matched, err := s.store.MatchSecret(ctx, projectID, secretType, host)
	if err != nil && !isAdvisoryMatchError(err) {
		return nil, err
	}
	if err == nil && matched.AutoApprove {
		now := time.Now().UTC()
		exp := now.Add(time.Duration(matched.DefaultGrantTTL) * time.Second)
		req.SecretID = matched.ID
		req.Status = model.SecretRequestStatusApproved
		req.ApprovedBy = "auto"
		req.GrantedAt = &now
		req.ExpiresAt = &exp
	}

	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}

	return s.populateValue(ctx, req)
}

func (s *Service) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apiError(err, "secret request not found")
	}
	return s.populateValue(ctx, req)
}

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

	ttl := secret.DefaultGrantTTL
	if v, ok := input.GrantTTLSeconds.Get(); ok && v > 0 {
		ttl = v
	}

	principal, _ := auth.PrincipalFromContext(ctx)
	approvedBy := principal.UserID
	if approvedBy == "" {
		approvedBy = principal.WorkerID
	}

	now := time.Now().UTC()
	exp := now.Add(time.Duration(ttl) * time.Second)
	req.SecretID = secret.ID
	req.Status = model.SecretRequestStatusApproved
	req.ApprovedBy = approvedBy
	req.GrantedAt = &now
	req.ExpiresAt = &exp

	if err := s.store.UpdateSecretRequestIfPending(ctx, req); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
		}
		return nil, err
	}

	return s.populateValue(ctx, req)
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

// ResolveSandboxSecret resolves a sentinel injected into a sandbox to its secret
// grant for a destination host. It maps the sentinel to its assignment and reuses
// the secret-request approval flow: an auto-approved or previously-approved,
// unexpired grant returns a request carrying the decrypted value; otherwise a
// pending request is created (or an existing one reused) and the returned request
// has no value, so the proxy leaves the sentinel in place until approval.
//
// requestedBy is scoped to the sandbox so grants and pending requests do not
// leak across sandboxes.
func (s *Service) ResolveSandboxSecret(ctx context.Context, workerID, sandboxID, sentinel, host string) (*model.SecretRequest, error) {
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
	requestedBy := "sandbox:" + sandboxID
	host = strings.TrimSpace(host)

	existing, err := s.store.FindLatestSecretRequest(ctx, assignment.ProjectID, assignment.SecretID, host, requestedBy)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		switch existing.Status {
		case model.SecretRequestStatusApproved:
			if existing.ExpiresAt == nil || time.Now().UTC().Before(*existing.ExpiresAt) {
				return s.populateValue(ctx, existing)
			}
			// Grant expired; fall through to create a fresh request.
		case model.SecretRequestStatusPending:
			return existing, nil
		}
	}

	req := &model.SecretRequest{
		ProjectID:   assignment.ProjectID,
		RequestedBy: requestedBy,
		SandboxID:   assignment.SandboxID,
		Type:        secret.Type,
		Host:        host,
		SecretID:    secret.ID,
		Status:      model.SecretRequestStatusPending,
	}
	if secret.AutoApprove {
		now := time.Now().UTC()
		exp := now.Add(time.Duration(secret.DefaultGrantTTL) * time.Second)
		req.Status = model.SecretRequestStatusApproved
		req.ApprovedBy = "auto"
		req.GrantedAt = &now
		req.ExpiresAt = &exp
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return s.populateValue(ctx, req)
}

// populateValue decrypts and attaches the secret value when the request is
// approved and the grant has not expired.
func (s *Service) populateValue(ctx context.Context, req *model.SecretRequest) (*model.SecretRequest, error) {
	if req.Status != model.SecretRequestStatusApproved || req.SecretID == "" {
		return req, nil
	}
	if req.ExpiresAt != nil && time.Now().UTC().After(*req.ExpiresAt) {
		return req, nil
	}
	sec, err := s.store.GetSecret(ctx, req.ProjectID, req.SecretID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Secret was deleted after approval; return the request without a value.
			return req, nil
		}
		return nil, fmt.Errorf("load secret for grant: %w", err)
	}
	val, err := s.store.OpenSecretValue(ctx, sec)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	req.Value = val
	return req, nil
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
