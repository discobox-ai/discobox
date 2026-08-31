package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/hostscope"
	"github.com/discobox-ai/discobox/secretformat"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// defaultMaxGrantTTLSeconds is the limit a secret gets when its creator
// names none: an hour, which is short enough that forgetting to think about it
// is not the same as granting forever.
const defaultMaxGrantTTLSeconds = 3600

type Service struct {
	store *store.Store
	// oauthRefresh collapses concurrent resolves of the same OAuth secret onto a
	// single upstream token refresh, so a rotating refresh token is spent once.
	oauthRefresh singleflight.Group
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apiError(err, "project not found")
	}
	secrets, err := s.store.ListSecrets(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for i := range secrets {
		s.describeOAuth(ctx, &secrets[i])
	}
	return secrets, nil
}

// describeOAuth fills in what an OAuth credential is, for a caller that may not
// see what it is: where it renews, which client it belongs to, what it may do,
// and when the access token goes stale.
//
// It reads the encrypted value to do it, because that is where the metadata was
// captured with the tokens. Nothing it copies out could be used as the
// credential — a failure to decrypt leaves the summary empty rather than
// failing the read, since a secret whose key has moved on is still a row worth
// listing.
func (s *Service) describeOAuth(ctx context.Context, secret *model.Secret) {
	if secret == nil || secret.Type != model.SecretTypeOAuth {
		return
	}
	value, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil || value == nil {
		return
	}
	secret.OAuth = &model.SecretOAuth{
		TokenURL:             value.TokenURL,
		ClientID:             value.ClientID,
		Scopes:               value.Scopes,
		SubscriptionType:     value.SubscriptionType,
		AccessTokenExpiresAt: value.AccessTokenExpiresAt,
		Refreshable:          value.RefreshToken != "" && value.TokenURL != "",
	}
}

// normalizeHost is the one reading of a destination host in this package.
//
// The proxy reports the host it observed lowercased and without a port, and a
// grant is matched against that string by SQL equality. So a host stored any
// other way is a grant that can never match: `--host API.github.com` mints an
// approval nothing will ever use, and the failure looks like a revoked
// credential rather than a typo. Normalizing on the way in is what keeps the
// stored host and the observed one comparable.
func normalizeHost(host string) string { return hostscope.Normalize(host) }

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
	// An explicit zero is honored, unlike every other unset field here: zero is
	// the meaningful value "no limit", and a caller who types it is saying
	// grants on this credential may live forever.
	ttl := int64(defaultMaxGrantTTLSeconds)
	if v, ok := input.MaxGrantTTLSeconds.Get(); ok {
		if v < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a grant limit is a number of seconds; 0 allows grants that never expire")
		}
		ttl = v
	}
	host := ""
	if v, ok := input.Host.Get(); ok {
		host = strings.TrimSpace(v)
	}
	format := ""
	if secretType == model.SecretTypeToken {
		// The shape is read from the value; the host is not. What a credential
		// is for is a binding somebody sets, and a secret nobody bound is
		// usable wherever a grant says — which is the field that decides it.
		if token := strings.TrimSpace(input.Value.Token.Or("")); token != "" {
			format = secretformat.Describe(token)
		}
	}
	if err := checkOAuthValue(secretType, input.Value); err != nil {
		return nil, err
	}
	sec := &model.Secret{
		ProjectID:      projectID,
		Name:           name,
		Type:           secretType,
		Host:           normalizeHost(host),
		Format:         format,
		MaxGrantTTL:    ttl,
		EncryptedValue: valueBytes,
	}
	if err := s.store.CreateSecret(ctx, sec); err != nil {
		return nil, secretCollision(err, sec)
	}
	// Through the service's own read, so a create and an update answer with the
	// same summary a get does rather than a bare row.
	return s.GetSecret(ctx, projectID, sec.ID)
}

func (s *Service) GetSecret(ctx context.Context, projectID, secretID string) (*model.Secret, error) {
	sec, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	s.describeOAuth(ctx, sec)
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
		sec.Host = normalizeHost(hostVal)
	}
	if ttl, ok := input.MaxGrantTTLSeconds.Get(); ok {
		if ttl < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a grant limit is a number of seconds; 0 allows grants that never expire")
		}
		// Lowering the limit binds the grants minted after it, not the ones
		// already standing: a live grant is an authorization somebody made,
		// and it is revoked deliberately rather than shortened behind their
		// back.
		sec.MaxGrantTTL = ttl
	}
	if valueVal, ok := input.Value.Get(); ok {
		valueBytes, err := marshalSecretValue(valueVal)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
		}
		sec.EncryptedValue = valueBytes
		if sec.Type == model.SecretTypeToken {
			if token := strings.TrimSpace(valueVal.Token.Or("")); token != "" {
				sec.Format = secretformat.Describe(token)
			}
		}
	}
	if err := s.store.UpdateSecret(ctx, sec); err != nil {
		return nil, secretCollision(err, sec)
	}
	// Through the service's own read, so a create and an update answer with the
	// same summary a get does rather than a bare row.
	return s.GetSecret(ctx, projectID, sec.ID)
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
		host = normalizeHost(v)
	}

	principal, ok := auth.PrincipalFromContext(ctx)
	if !ok {
		return nil, apperrors.NewStatusError(http.StatusUnauthorized, "authentication required")
	}
	requestedBy := principal.UserID
	if requestedBy == "" {
		requestedBy = principal.PoolID
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
	host := normalizeHost(input.Host.Or(req.Host))

	// A protocol-originated request is a different species from one the proxy
	// minted on hitting an unresolvable sentinel, and the two are handled apart
	// rather than merged (ADR 0031 §5). Approving one carries obligations a
	// reactive approval does not have: it must be scoped to a concrete host and
	// to the asking sandbox, it mints approved uses, and it binds a stable
	// sentinel the agent never sees.
	var approvedUses []model.SecretUse
	if req.FromProtocol() {
		if host == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"approving an agent credential request requires a host; a wildcard grant must be created explicitly with `discobox secret grant create`")
		}
		if scope != model.SecretGrantScopeSandbox {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"an agent credential request can only be approved at sandbox scope")
		}
		requested := req.Uses
		if edited, ok := input.Uses.Get(); ok && len(edited) > 0 {
			requested = convertAPIUses(edited)
		}
		approvedUses, err = mintUseIDs(requested)
		if err != nil {
			return nil, err
		}
		if len(approvedUses) == 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "approving an agent credential request requires at least one use")
		}
	}

	scopeKey, err := s.grantScopeKey(ctx, projectID, req.SandboxID, scope)
	if err != nil {
		return nil, err
	}

	// The secret's limit is also the lifetime nobody has to choose; an explicit
	// value is checked against it in mintGrantAs, along with every other path
	// that mints one.
	ttl := secret.MaxGrantTTL
	if v, ok := input.GrantTTLSeconds.Get(); ok {
		ttl = v
	}
	grant, err := s.mintGrantAs(ctx, projectID, secret, scope, scopeKey, host, req.EnvName, ttl, approvedUses)
	if err != nil {
		return nil, err
	}

	if req.FromProtocol() {
		if err := s.bindAgentCredential(ctx, req, secret); err != nil {
			// Leave no live authorization behind for a binding that never
			// happened: without the binding nothing can be activated, so the
			// grant would be an approval nobody can act on and nobody can see.
			_ = s.store.DeleteSecretGrant(ctx, projectID, grant.ID)
			return nil, err
		}
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
// any scope (its own ID, its harness config, or the project). A match returns the
// decrypted value; otherwise a single pending request is created (or reused) and
// the proxy leaves the sentinel in place until a grant exists.
func (s *Service) ResolveSandboxSecret(ctx context.Context, poolID, sandboxID, sentinel, host string) (*model.SandboxSecretResolution, error) {
	assignment, err := s.store.GetSandboxSecretBySentinel(ctx, sandboxID, sentinel)
	if err != nil {
		return nil, apiError(err, "sandbox secret not found")
	}
	// The calling pool agent must own the sandbox the sentinel belongs to.
	sandbox, err := s.store.GetSandbox(ctx, assignment.ProjectID, assignment.SandboxID)
	if err != nil {
		return nil, apiError(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox secret not found")
	}
	secret, err := s.store.GetSecret(ctx, assignment.ProjectID, assignment.SecretID)
	if err != nil {
		return nil, apiError(err, "secret not found")
	}
	host = normalizeHost(host)

	scopes := []store.GrantScope{
		{Scope: model.SecretGrantScopeSandbox, ScopeKey: sandbox.ID},
		{Scope: model.SecretGrantScopeProject, ScopeKey: assignment.ProjectID},
	}
	if sandbox.HarnessConfigID != nil && strings.TrimSpace(*sandbox.HarnessConfigID) != "" {
		scopes = append(scopes, store.GrantScope{Scope: model.SecretGrantScopeHarnessConfig, ScopeKey: strings.TrimSpace(*sandbox.HarnessConfigID)})
	}
	// The binding is checked where the credential is handed out, not only where
	// a grant is minted. A secret bound to a host may be used for that host and
	// the hosts beneath it and nowhere else — which has to hold for grants that
	// already exist too: one written before the binding, or before this check,
	// is exactly the grant nobody would write today.
	if !hostscope.Covers(secret.Host, host) {
		return &model.SandboxSecretResolution{Status: model.SecretRequestStatusDenied}, nil
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
		expiresAt := grant.ExpiresAt
		if secret.Type == model.SecretTypeOAuth {
			// Refresh a near-expired access token before handing it out, and cap the
			// cache lifetime the proxy honors by the token's own expiry so it
			// re-resolves — and re-refreshes — as the token ages out.
			val, err = s.ensureFreshOAuth(ctx, secret, val)
			if err != nil {
				return nil, err
			}
			expiresAt = oauthResolutionExpiry(grant.ExpiresAt, val)
		}
		return &model.SandboxSecretResolution{Status: model.SecretRequestStatusApproved, Value: val, ExpiresAt: expiresAt}, nil
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
	host := normalizeHost(input.Host.Or(secret.Host))
	// Default to the secret's limit; an explicit value wins, up to that limit.
	ttl := secret.MaxGrantTTL
	if v, ok := input.GrantTTLSeconds.Get(); ok {
		ttl = v
	}

	// Uses make this the agent credentials shape: a credential nothing in the
	// sandbox can read, which the in-sandbox CLI takes one use at a time. It
	// carries the obligations approving an agent's request carries, for the
	// same reasons (ADR 0031 §§4–5), and it is the same binding underneath.
	envVar := strings.TrimSpace(input.EnvVar.Or(""))
	var uses []model.SecretUse
	if declared, ok := input.Uses.Get(); ok && len(declared) > 0 {
		if host == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"a grant with uses requires a host; a wildcard grant stays an explicit administrative act")
		}
		if envVar == "" || strings.ContainsAny(envVar, "=\x00") {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"a grant with uses requires the environment variable the agent receives it in")
		}
		uses, err = mintUseIDs(convertAPIUses(declared))
		if err != nil {
			return nil, err
		}
		if len(uses) == 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "each declared use requires a description")
		}
	} else if envVar != "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"an environment variable is only meaningful with uses: without them the grant authorizes the sentinel the sandbox already holds")
	}

	grant, err := s.mintGrantAs(ctx, projectID, secret, scope, scopeKey, host, envVar, ttl, uses)
	if err != nil {
		return nil, err
	}
	// A grant on one discobox binds now, because there is a discobox to bind
	// to and a failure is worth reporting to whoever is granting it. A wider
	// one binds per discobox the first time that discobox's agent asks: the
	// boxes it covers may not exist yet.
	if len(uses) > 0 && scope == model.SecretGrantScopeSandbox {
		if err := s.bindAgentSecret(ctx, projectID, scopeKey, envVar, secret); err != nil {
			// Leave no live authorization behind for a binding that never
			// happened, exactly as approving one does.
			_ = s.store.DeleteSecretGrant(ctx, projectID, grant.ID)
			return nil, err
		}
	}
	return grant, nil
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
	case model.SecretGrantScopeHarnessConfig:
		if sandboxID == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "harnessConfig scope requires a sandbox-originated request")
		}
		sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
		if err != nil {
			return "", apiError(err, "sandbox not found")
		}
		if sandbox.HarnessConfigID == nil || strings.TrimSpace(*sandbox.HarnessConfigID) == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "sandbox has no harness config to scope the grant to")
		}
		return strings.TrimSpace(*sandbox.HarnessConfigID), nil
	default:
		return "", apperrors.NewStatusError(http.StatusBadRequest, "grant scope must be sandbox, harnessConfig, or project")
	}
}

// guardGrantHost refuses a grant that would send a credential somewhere it does
// not belong.
//
// The grant's host is what the proxy enforces; the secret's own host says which
// service the credential is *for*. When both are set and they disagree, the
// grant would swap that credential into requests to another service — an
// approval typo becomes the real key leaving for a host that was never supposed
// to see it, and with the agent credentials flow the host is proposed by the
// sandbox. A secret carrying no host is unconstrained on purpose: the field is
// often inferred from the token's shape, and a credential that genuinely spans
// hosts is expressed by leaving it empty rather than by widening every grant.
func guardGrantHost(secret *model.Secret, host string) error {
	secretHost := normalizeHost(secret.Host)
	// The grant has to sit inside the binding, not merely touch it: a secret
	// for github.com may be granted for api.github.com, and one for
	// api.github.com may not be granted for github.com — that is a different
	// host, serving different things, and the binding says the credential does
	// not belong to it.
	if secretHost == "" || hostscope.Covers(secretHost, host) {
		return nil
	}
	// The message names the command, the way the wildcard-grant refusal does:
	// the remedy is a second call, and an error that describes one without
	// spelling it is an error the reader has to go looking behind.
	if host == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
			"secret %s is bound to %s, so it cannot be granted for every host; grant it for %s, or release the binding with `discobox secret update %s --host \"\"`",
			secret.ID, secretHost, secretHost, secret.ID))
	}
	// A binding that covers both is usually the right answer rather than none:
	// a credential asked for at github.com and api.github.com belongs to the
	// site, and the site covers what is beneath it.
	remedy := fmt.Sprintf("`discobox secret update %s --host %s`", secret.ID, hostscope.CommonParent(secretHost, host))
	if hostscope.CommonParent(secretHost, host) == "" {
		remedy = fmt.Sprintf("`discobox secret update %s --host \"\"`", secret.ID)
	}
	return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
		"secret %s is bound to %s and cannot be granted for %s; pick a secret for %s, or widen the binding with %s",
		secret.ID, secretHost, host, host, remedy))
}

// guardGrantTTL refuses a grant that would outlive what the secret allows.
//
// The limit is the one place a credential says how long consent to it may last,
// and it has to bite at minting rather than at creation: the lifetime arrives
// from an approval dialog, a pre-approval, or the in-sandbox flow, and a rule
// enforced in one of those is a rule the other two walk around. A secret with
// no limit is unconstrained on purpose — that is what zero says — and there the
// grant keeps whatever lifetime it was given, forever included.
func guardGrantTTL(secret *model.Secret, ttlSeconds int64) error {
	limit := secret.MaxGrantTTL
	if limit <= 0 || (ttlSeconds > 0 && ttlSeconds <= limit) {
		return nil
	}
	// The remedy names the command, as the host refusal does: raising the limit
	// is a deliberate act on the secret, not something a grant may do in
	// passing by asking for more.
	if ttlSeconds <= 0 {
		return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
			"a grant on secret %s may live at most %s, so it cannot be granted forever; grant it for less, or lift the limit with `discobox secret update %s --max-grant-ttl 0`",
			secret.ID, formatTTL(limit), secret.ID))
	}
	return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
		"a grant on secret %s may live at most %s, and %s was asked for; grant it for less, or raise the limit with `discobox secret update %s --max-grant-ttl %d`",
		secret.ID, formatTTL(limit), formatTTL(ttlSeconds), secret.ID, ttlSeconds))
}

// formatTTL says a lifetime the way a person reads one, so a refusal compares
// two durations rather than two integers.
func formatTTL(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

// mintGrant creates the standing authorization. uses is non-empty only for a
// grant minted by approving an agent credentials protocol request; a plain
// grant authorizes the credential without enumerating what it is for.
//
// It takes the secret rather than its ID because it is the one place every
// grant passes through, which makes it the place to check the grant against the
// credential it hands out.
// mintGrantAs is mintGrant with the environment variable a grant carrying uses
// delivers in. It is on the grant because a grant wider than one discobox has
// no binding yet, and the binding is what the name would otherwise live on.
func (s *Service) mintGrantAs(ctx context.Context, projectID string, secret *model.Secret, scope, scopeKey, host, envName string, ttlSeconds int64, uses []model.SecretUse) (*model.SecretGrant, error) {
	host = normalizeHost(host)
	if err := guardGrantHost(secret, host); err != nil {
		return nil, err
	}
	if err := guardGrantTTL(secret, ttlSeconds); err != nil {
		return nil, err
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	grantedBy := principal.UserID
	if grantedBy == "" {
		grantedBy = principal.PoolID
	}
	grant := &model.SecretGrant{
		ProjectID: projectID,
		SecretID:  secret.ID,
		Scope:     scope,
		ScopeKey:  scopeKey,
		Host:      host,
		GrantedBy: grantedBy,
		Uses:      uses,
		EnvName:   strings.TrimSpace(envName),
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
	case model.SecretGrantScopeSandbox, model.SecretGrantScopeHarnessConfig, model.SecretGrantScopeProject:
	default:
		return apperrors.NewStatusError(http.StatusBadRequest, "grant scope must be sandbox, harnessConfig, or project")
	}
	if scopeKey == "" {
		return apperrors.NewStatusError(http.StatusBadRequest, "grant scopeKey is required")
	}
	return nil
}

// marshalSecretValue converts a generated SecretValue to JSON plaintext for encryption.
func marshalSecretValue(val apigen.SecretValue) ([]byte, error) {
	mv := model.SecretValue{
		Token:                val.Token.Or(""),
		RefreshToken:         val.RefreshToken.Or(""),
		TokenURL:             strings.TrimSpace(val.TokenUrl.Or("")),
		ClientID:             strings.TrimSpace(val.ClientId.Or("")),
		AccessTokenExpiresAt: val.AccessTokenExpiresAt.Or(0),
		SubscriptionType:     strings.TrimSpace(val.SubscriptionType.Or("")),
	}
	if scopes, ok := val.Scopes.Get(); ok {
		mv.Scopes = scopes
	}
	//nolint:gosec // Secret values are intentionally marshaled before store encryption.
	return json.Marshal(mv)
}

// checkOAuthValue holds the two types apart. An oauth secret is one that can
// renew itself: the control plane spends the refresh token at the token URL as
// the access token ages out (ADR 0011). Without both, what has been handed over
// is a token that will expire and stay expired, and calling it oauth would
// promise a refresh nothing can perform.
func checkOAuthValue(secretType string, val apigen.SecretValue) error {
	if secretType != model.SecretTypeOAuth {
		return nil
	}
	if strings.TrimSpace(val.RefreshToken.Or("")) == "" || strings.TrimSpace(val.TokenUrl.Or("")) == "" {
		return apperrors.NewStatusError(http.StatusBadRequest,
			"an oauth secret is one that renews itself: give it a refreshToken and a tokenUrl, or store it as a token")
	}
	return nil
}

func validSecretType(t string) bool {
	return t == model.SecretTypeToken || t == model.SecretTypeOAuth
}

// isGenerationConflict reports whether err is the store's optimistic-concurrency
// signal, used by the OAuth refresh to detect that another writer rotated first.
func isGenerationConflict(err error) bool {
	return errors.Is(err, store.ErrGenerationConflict)
}

// secretCollision turns the uniqueness constraint into an answer. A name
// already taken for that host is something the caller can fix; a driver's
// constraint text reaching the client as a 500 is neither readable nor true —
// nothing failed on the server's side.
func secretCollision(err error, sec *model.Secret) error {
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
		return err
	}
	where := "with no host"
	if sec.Host != "" {
		where = "for " + sec.Host
	}
	return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
		"this project already has a %s secret named %q %s; pick another name, or update the one it has",
		sec.Type, sec.Name, where))
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
