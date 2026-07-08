package sandboxes

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/obot-platform/discobox/id"
	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/secretformat"
	services "github.com/obot-platform/discobox/server/internal/services"
)

const defaultSentinelFormat = "{alnum:48}"

// prepareSandboxSecrets resolves each secret input to a project secret (creating
// an anonymous secret for inline values), mints a sentinel placeholder from the
// secret's format, injects it into the sandbox environment, and returns the
// assignment rows to persist after the sandbox is created. It mutates sandbox.Env.
func (s *Service) prepareSandboxSecrets(ctx context.Context, projectID string, sandbox *model.Sandbox, inputs []services.SandboxSecretInput) ([]*model.SandboxSecret, error) {
	if len(inputs) == 0 {
		return nil, nil
	}
	if sandbox.Env == nil {
		sandbox.Env = map[string]string{}
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
		sandbox.Env[env] = sentinel
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
	secretID, err := id.New()
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
