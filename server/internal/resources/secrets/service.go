package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"

	apigen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/secretformat"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// defaultMaxBindingTTLSeconds is the limit a secret gets when its creator
// names none: an hour, which is short enough that forgetting to think about it
// is not the same as binding forever.
const defaultMaxBindingTTLSeconds = 3600

type Service struct {
	store *store.Store
	// renewing collapses concurrent resolves of the same OAuth or exchange
	// secret onto a single upstream renewal, so a rotating refresh token is
	// spent once.
	renewing singleflight.Group
	// renewals is when a rejection last forced a renewal of each credential,
	// which bounds how often a refused credential may spend a refresh token.
	// See renewedRecently.
	renewalsMu sync.Mutex
	renewals   map[string]time.Time
	// holders marks the sentinel delivery of the discoboxes a change reaches,
	// in the transaction that makes it, so each is sent the result.
	holders SecretHolders
}

// SecretHolders marks the sentinel delivery of discoboxes for reconcile,
// inside the caller's transaction, so what a change gives or takes away
// reaches each of them (ADR 26-10-27-880 §2). The sandbox service is one; it
// is set after construction because it is built from this service.
type SecretHolders interface {
	// MarkSecretHoldersDirtyTx marks every discobox holding a secret, in the
	// transaction that changed its routes or removed it.
	MarkSecretHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID, secretID string) error
	// MarkBindingHoldersDirtyTx marks every discobox the bindings' targets
	// cover, in the transaction that made, changed or removed them.
	MarkBindingHoldersDirtyTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, projectID string, bindings []model.SecretBinding) error
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

// SetSecretHolders gives the service what marks a secret's holders for
// reconcile when its routes change.
func (s *Service) SetSecretHolders(holders SecretHolders) {
	s.holders = holders
}

// routesFrom reads the routes a body names (ADR 26-10-02-924, ADR
// 26-10-27-939). A route without a host, or with a malformed path, is refused
// rather than dropped: a person who wrote one meant somewhere.
func routesFrom(input []apigen.SecretRouteInput) ([]model.SecretRoute, error) {
	routes := make([]model.SecretRoute, 0, len(input))
	for _, route := range input {
		routes = append(routes, model.SecretRoute{Host: route.Host, Path: route.Path.Or(""), Blocked: route.Blocked.Or(false)})
	}
	normalized, err := model.NormalizeRoutes(routes)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return normalized, nil
}

// replaceRoutes writes the routes a create or update names onto sec.
func replaceRoutes(sec *model.Secret, input []apigen.SecretRouteInput) (bool, error) {
	routes, err := routesFrom(input)
	if err != nil {
		return false, err
	}
	changed, err := sec.ReplaceRoutes(routes)
	if err != nil {
		return false, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return changed, nil
}

func (s *Service) ListSecrets(ctx context.Context, projectID string) ([]model.Secret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	// A discobox sees only the secrets it was delegated: the ones it may hand
	// on, and so the only ones it may name (ADR 26-10-27-880 §3).
	var options []store.SecretListOption
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		sandbox, err := s.store.GetSandbox(ctx, projectID, principal.SandboxID)
		if err != nil {
			return nil, apperrors.NotFound(err, "sandbox not found")
		}
		options = append(options, store.DelegatedTo(store.SandboxBindingTargets(sandbox)))
	}
	secrets, err := s.store.ListSecrets(ctx, projectID, options...)
	if err != nil {
		return nil, err
	}
	for i := range secrets {
		s.describeOAuth(ctx, &secrets[i])
		s.describeExchange(ctx, &secrets[i])
		describeStaleness(&secrets[i])
		withholdRenewalFromSandbox(ctx, &secrets[i])
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

// normalizeHost is the one reading of a destination host in this package:
// lowercased and without a port, as the proxy reports the host it observed.
func normalizeHost(host string) string { return hostscope.Normalize(host) }

func (s *Service) CreateSecret(ctx context.Context, projectID string, input services.CreateSecretBody) (*model.Secret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
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
	// bindings of this credential may live forever.
	ttl := int64(defaultMaxBindingTTLSeconds)
	if v, ok := input.MaxBindingTTLSeconds.Get(); ok {
		if v < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding limit is a number of seconds; 0 allows bindings that never expire")
		}
		ttl = v
	}
	if err := checkOAuthValue(secretType, input.Value); err != nil {
		return nil, err
	}
	format, formatSet, err := sentinelFormat(input.Format)
	if err != nil {
		return nil, err
	}
	sec := &model.Secret{
		ProjectID:      projectID,
		Name:           name,
		Type:           secretType,
		Format:         format,
		FormatSet:      formatSet,
		MaxBindingTTL:  ttl,
		EncryptedValue: valueBytes,
	}
	sec.ValueWritten(time.Now().UTC(), nil)
	// Routes first: a recipe is held to them. A secret with none is sent
	// nowhere (ADR 26-10-27-880 §1); `*` is how a person says anywhere.
	if routes, ok := input.Routes.Get(); ok {
		if _, err := replaceRoutes(sec, routes); err != nil {
			return nil, err
		}
	}
	if sec.ExchangeRecipe, err = exchangeRecipe(ctx, secretType, sec.Routes, input.Exchange); err != nil {
		return nil, err
	}
	if sec.Type == model.SecretTypeExchange {
		// Exchanged before it is stored, so a key the endpoint refuses is
		// never stored (ADR 26-10-08-452 §3).
		if sec.EncryptedValue, err = firstExchange(ctx, sec, input.Value); err != nil {
			return nil, err
		}
	}
	if err := createLifetime(input).apply(sec, true); err != nil {
		return nil, err
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
		return nil, apperrors.NotFound(err, "secret not found")
	}
	s.describeOAuth(ctx, sec)
	s.describeExchange(ctx, sec)
	describeStaleness(sec)
	withholdRenewalFromSandbox(ctx, sec)
	return sec, nil
}

func (s *Service) UpdateSecret(ctx context.Context, projectID, secretID string, input services.UpdateSecretBody) (*model.Secret, error) {
	sec, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret not found")
	}
	if nameVal, ok := input.Name.Get(); ok {
		name := strings.TrimSpace(nameVal)
		if name == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret name is required")
		}
		sec.Name = name
	}
	if input.Format.IsSet() {
		// Cleared, the store reads the shape from the value again on this
		// write, the way it does for a value nobody described.
		if sec.Format, sec.FormatSet, err = sentinelFormat(input.Format); err != nil {
			return nil, err
		}
	}
	if ttl, ok := input.MaxBindingTTLSeconds.Get(); ok {
		if ttl < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding limit is a number of seconds; 0 allows bindings that never expire")
		}
		// Lowering the limit binds the bindings made after it, not the ones
		// already standing: a live binding is an authorization somebody made,
		// and it is removed deliberately rather than shortened behind their
		// back.
		sec.MaxBindingTTL = ttl
	}
	if err := updateLifetime(input).apply(sec, input.Value.IsSet()); err != nil {
		return nil, err
	}
	routesChanged := false
	if routes, ok := input.Routes.Get(); ok {
		if routesChanged, err = replaceRoutes(sec, routes); err != nil {
			return nil, err
		}
	}
	// A recipe says where a stored key is sent, so it changes only with the
	// key beside it: pointing a stored key at a new endpoint would hand it to
	// whoever runs that endpoint. A new binding is held to the recipe too.
	recipe, err := exchangeRecipe(ctx, sec.Type, sec.Routes, input.Exchange)
	switch {
	case err != nil:
		return nil, err
	case recipe != nil && !input.Value.IsSet():
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a new exchange recipe is given with the value it exchanges: the stored fields are not sent anywhere new")
	case recipe != nil:
		sec.ExchangeRecipe = recipe
	case sec.ExchangeRecipe != nil && routesChanged:
		if err := checkRecipe(sec.ExchangeRecipe, sec.Routes); err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
		}
	}
	if valueVal, ok := input.Value.Get(); ok {
		valueBytes, err := marshalSecretValue(valueVal)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
		}
		if sec.Type == model.SecretTypeExchange {
			if valueBytes, err = firstExchange(ctx, sec, valueVal); err != nil {
				return nil, err
			}
		}
		sec.EncryptedValue = valueBytes
	}
	// A replaced value retracts what was recorded about the credential it
	// replaced (ADR 0132 §4). The store does it, for every writer at once —
	// this one and the harness configure flow's update-in-place — rather than
	// each of them remembering to.
	//
	// A running discobox's proxy holds its swaps to the routes it was last
	// handed, so a change to them marks the sentinel delivery of every
	// discobox holding the secret in the same transaction: each is sent the
	// routes as written, and one that cannot be reached now is retried.
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.UpdateSecret(ctx, sec); err != nil {
			return secretCollision(err, sec)
		}
		if routesChanged && s.holders != nil {
			return s.holders.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, projectID, sec.ID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	// Through the service's own read, so a create and an update answer with the
	// same summary a get does rather than a bare row.
	return s.GetSecret(ctx, projectID, sec.ID)
}

// DeleteSecret removes a secret with its bindings, each recorded as removed,
// and the sentinels that delivered it. Every discobox that held it is marked
// in the same transaction, so its pool stops holding a sentinel nothing
// resolves.
func (s *Service) DeleteSecret(ctx context.Context, projectID, secretID string) error {
	sec, err := s.store.GetSecret(ctx, projectID, secretID)
	if err != nil {
		return apperrors.NotFound(err, "secret not found")
	}
	return s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if s.holders != nil {
			if err := s.holders.MarkSecretHoldersDirtyTx(ctx, txStore, txDB, projectID, sec.ID); err != nil {
				return err
			}
		}
		if err := txStore.DeleteSecret(ctx, projectID, sec.ID); err != nil {
			return apperrors.NotFound(err, "secret not found")
		}
		return nil
	})
}

func (s *Service) ListSecretRequests(ctx context.Context, projectID, status string) ([]model.SecretRequest, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	// A discobox sees only the requests it owns: those filed by a discobox it
	// created (ADR 26-09-30-782 §2). The sandbox role answers whether it may
	// reach one by its ID; a listing it filters here.
	var options []store.SecretRequestListOption
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		options = append(options, store.OwnedBy(principal.SandboxID))
	}
	return s.store.ListSecretRequests(ctx, projectID, status, options...)
}

func (s *Service) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret request not found")
	}
	return req, nil
}

// errRequestAnswered is an approval that found its request no longer pending.
var errRequestAnswered = errors.New("the request was answered concurrently")

// ApproveSecretRequest answers a pending request with a binding on the
// discobox that asked (ADR 26-10-27-880 §5): to use the secret chosen, in the
// variable it named, or to delegate it. The binding, its event, the mark that
// delivers it and the request's decision are one transaction.
//
// A person may answer any request. A discobox answers a use request from a
// discobox it created, and only under a live delegate binding covering it of
// the secret it answers with (§3): the binding is handed on under that one,
// as its child, and lasts no longer.
func (s *Service) ApproveSecretRequest(ctx context.Context, projectID, requestID string, input services.ApproveSecretRequestBody) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusPending {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}
	if req.SandboxID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "this request names no discobox to bind the credential to")
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	approverIsSandbox := principal.Type == auth.PrincipalTypeSandbox

	// The routes the approver named, else the ones the request asked for.
	routes := req.Routes
	if input.Routes != nil {
		if routes, err = routesFrom(input.Routes); err != nil {
			return nil, err
		}
	}
	ttl, ttlNamed := input.TtlSeconds.Get()
	if ttlNamed && ttl < 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding's lifetime is a number of seconds; 0 is forever")
	}
	secretRef := strings.TrimSpace(input.SecretId.Or(""))

	write := &approvalWrite{req: req, routes: routes, ttl: ttl, ttlNamed: ttlNamed, actor: principal.Actor()}
	if approverIsSandbox {
		if req.Delegates() {
			return nil, apperrors.NewStatusError(http.StatusForbidden,
				"a discobox never approves a request to delegate: handing on the power to hand on is a person's to approve")
		}
		parents, secret, err := s.delegationsFor(ctx, projectID, principal.SandboxID, secretRef)
		if err != nil {
			return nil, err
		}
		write.secret, write.parents, write.approverID = secret, parents, principal.SandboxID
	} else {
		if secretRef == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required")
		}
		if write.secret, err = s.lookupSecret(ctx, projectID, secretRef); err != nil {
			return nil, err
		}
	}
	if err := s.commitApproval(ctx, write); err != nil {
		if errors.Is(err, errRequestAnswered) {
			return nil, apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
		}
		return nil, err
	}
	if write.approverID != "" {
		slog.InfoContext(ctx, "a discobox approved a request under its delegate binding",
			"projectId", projectID, "requestId", req.ID, "bindingId", req.BindingID,
			"approver", write.approverID, "parentBindingId", write.binding.ParentID)
	}
	return req, nil
}

// approvalWrite is one approval of a request, as its transaction makes it.
type approvalWrite struct {
	req    *model.SecretRequest
	secret *model.Secret
	routes []model.SecretRoute
	// ttl is the lifetime the approver named, read only when ttlNamed.
	ttl      int64
	ttlNamed bool
	actor    string
	// parents are, for a discobox's approval, the delegate bindings it may
	// hand the secret on under, longest-lived first; approverID is the
	// discobox.
	parents    []model.SecretBinding
	approverID string
	// binding is what the approval made or extended.
	binding *model.SecretBinding
}

// commitApproval makes every write an approval makes as one act: the binding
// and its event, the mark that delivers it, and the request decided. A
// refusal anywhere — a variable already holding another credential, a
// delegate binding gone, the request answered concurrently — leaves none of
// them behind. A request no longer pending is errRequestAnswered.
func (s *Service) commitApproval(ctx context.Context, a *approvalWrite) error {
	req, projectID := a.req, a.req.ProjectID
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		secret, err := txStore.GetSecret(ctx, projectID, a.secret.ID)
		if err != nil {
			return apperrors.NotFound(err, "secret not found")
		}
		now := time.Now().UTC()
		ttl := a.ttl
		if !a.ttlNamed {
			ttl = defaultApprovalTTL(req, secret)
		}
		binding := &model.SecretBinding{
			ProjectID:  projectID,
			SecretID:   secret.ID,
			TargetKind: model.SecretBindingTargetSandbox,
			TargetID:   req.SandboxID,
			Use:        !req.Delegates(),
			Delegate:   req.Delegates(),
			Routes:     a.routes,
			RequestID:  req.ID,
			CreatedBy:  a.actor,
		}
		if binding.Use {
			binding.EnvName = req.EnvName
		}
		if a.approverID != "" {
			parent, err := heldParent(ctx, txStore, projectID, a.approverID, secret.ID, a.parents, now)
			if err != nil {
				return err
			}
			if ttl, err = fitToParent(parent, ttl, a.ttlNamed, now); err != nil {
				return err
			}
			binding.ParentID = parent.ID
		}
		if err := guardBindingTTL(secret, ttl); err != nil {
			return err
		}
		binding.ExpiresAt = expiryAt(now, ttl)
		made, err := s.putRequestedBinding(ctx, txStore, txDB, binding, store.BindingChange{Actor: a.actor, Cause: req.ID})
		if err != nil {
			return err
		}
		a.binding = made
		req.SecretID = secret.ID
		req.Status = model.SecretRequestStatusApproved
		req.BindingID = made.ID
		req.DecidedBy = a.actor
		req.DecidedAt = &now
		return txStore.UpdateSecretRequestIfPending(ctx, req)
	})
	if errors.Is(err, store.ErrGenerationConflict) {
		return errRequestAnswered
	}
	return err
}

// putRequestedBinding stores the binding an approval makes, or extends the
// one already standing for the same thing: a use of the same secret in the
// same variable, or a delegate binding of the same secret, takes the
// approval's routes and lifetime. A variable already holding another
// credential on the discobox is refused rather than taken over, since swapping
// the credential under a variable something already uses is the surprise this
// must not spring.
func (s *Service) putRequestedBinding(ctx context.Context, txStore *store.Store, txDB *gorm.DB, binding *model.SecretBinding, change store.BindingChange) (*model.SecretBinding, error) {
	existing, err := txStore.ListSecretBindings(ctx, binding.ProjectID, store.SecretBindingFilter{TargetKind: binding.TargetKind, TargetID: binding.TargetID})
	if err != nil {
		return nil, err
	}
	for i := range existing {
		standing := &existing[i]
		sameUse := binding.Use && standing.Use && standing.EnvName == binding.EnvName
		sameDelegate := binding.Delegate && standing.Delegate && standing.SecretID == binding.SecretID
		if !sameUse && !sameDelegate {
			continue
		}
		if standing.SecretID != binding.SecretID {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"%s already holds another credential in this discobox; approve the request with that secret, or ask for another variable", binding.EnvName))
		}
		if standing.ParentID != binding.ParentID {
			return nil, apperrors.NewStatusError(http.StatusConflict,
				"this discobox already holds that credential under another binding; remove it first")
		}
		standing.Routes, standing.ExpiresAt = binding.Routes, binding.ExpiresAt
		if err := txStore.UpdateSecretBinding(ctx, standing, change); err != nil {
			return nil, err
		}
		return standing, s.markBindingHolders(ctx, txStore, txDB, *standing)
	}
	if err := txStore.CreateSecretBinding(ctx, binding, change); err != nil {
		return nil, bindingConflict(err)
	}
	return binding, s.markBindingHolders(ctx, txStore, txDB, *binding)
}

// markBindingHolders marks the discoboxes bindings cover, when the service
// knows what marks them.
func (s *Service) markBindingHolders(ctx context.Context, txStore *store.Store, txDB *gorm.DB, bindings ...model.SecretBinding) error {
	if s.holders == nil || len(bindings) == 0 {
		return nil
	}
	return s.holders.MarkBindingHoldersDirtyTx(ctx, txStore, txDB, bindings[0].ProjectID, bindings)
}

func (s *Service) DenySecretRequest(ctx context.Context, projectID, requestID string) error {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return apperrors.NotFound(err, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusPending {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	now := time.Now().UTC()
	req.Status = model.SecretRequestStatusDenied
	req.DecidedBy = principal.Actor()
	req.DecidedAt = &now
	if err := s.store.UpdateSecretRequestIfPending(ctx, req); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
		}
		return err
	}
	return nil
}

// ResolveSandboxSecret resolves a sentinel injected into a sandbox. The
// sentinel names the binding that delivered it; the value is handed out only
// while that binding stands — live, of this secret, on a level covering the
// sandbox, and under a live parent when it was handed on — and only for a
// host every one of its route lists allows (ADR 26-10-27-880 §4). Anything
// else is refused: no request is opened, since a sentinel exists only because
// a binding does.
func (s *Service) ResolveSandboxSecret(ctx context.Context, poolID, sandboxID, sentinel, host string) (*model.SandboxSecretResolution, error) {
	assignment, err := s.store.GetSandboxSecretBySentinel(ctx, sandboxID, sentinel)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox secret not found")
	}
	// The calling pool agent must own the sandbox the sentinel belongs to.
	sandbox, err := s.store.GetSandbox(ctx, assignment.ProjectID, assignment.SandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox secret not found")
	}
	secret, err := s.store.GetSecret(ctx, assignment.ProjectID, assignment.SecretID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret not found")
	}
	host = normalizeHost(host)
	denied := &model.SandboxSecretResolution{Status: model.SecretRequestStatusDenied}

	binding, parent, ok, err := s.standingBinding(ctx, sandbox, assignment, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	if !ok || !secretroute.HostAllowed(store.RouteLists(secret, binding, parent), host) {
		return denied, nil
	}
	val, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	expiresAt := bindingExpiry(binding, parent)
	if renews(secret.Type) {
		// Renew a near-expired token before handing it out, and cap the
		// cache lifetime the proxy honors by the token's own expiry so it
		// re-resolves — and renews again — as the token ages out.
		val, err = s.ensureFresh(ctx, secret, val)
		if err != nil {
			return nil, err
		}
		expiresAt = tokenResolutionExpiry(expiresAt, val)
	}
	if secret.Renewable() {
		expiresAt = s.renewableResolution(ctx, secret, sandbox.ID, expiresAt)
	}
	reportScope, err := s.reportScope(ctx, sandbox, assignment.EnvName)
	if err != nil {
		return nil, err
	}
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusApproved, Value: val, ExpiresAt: expiresAt, SecretID: secret.ID, ReportScope: reportScope}, nil
}

// standingBinding is the binding a sentinel was delivered under and its
// parent, when it still stands for the sandbox at now: present, a use of the
// sentinel's secret, on a level covering the sandbox, live, and under a live
// parent when it has one.
func (s *Service) standingBinding(ctx context.Context, sandbox *model.Sandbox, assignment *model.SandboxSecret, now time.Time) (*model.SecretBinding, *model.SecretBinding, bool, error) {
	if assignment.BindingID == "" {
		return nil, nil, false, nil
	}
	binding, err := s.store.SecretBindingByExactID(ctx, sandbox.ProjectID, assignment.BindingID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !binding.Use || binding.SecretID != assignment.SecretID || !binding.Live(now) || !store.Covers(store.SandboxBindingTargets(sandbox), binding, false) {
		return nil, nil, false, nil
	}
	if binding.ParentID == "" {
		return binding, nil, true, nil
	}
	parent, err := s.store.SecretBindingByExactID(ctx, sandbox.ProjectID, binding.ParentID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, err
	}
	if !parent.Live(now) {
		return nil, nil, false, nil
	}
	return binding, parent, true, nil
}

// bindingExpiry is when a binding stops authorizing: its own expiry, or its
// parent's when that comes first.
func bindingExpiry(binding, parent *model.SecretBinding) *time.Time {
	expires := binding.ExpiresAt
	if parent != nil && parent.ExpiresAt != nil && (expires == nil || parent.ExpiresAt.Before(*expires)) {
		expires = parent.ExpiresAt
	}
	return expires
}

// guardBindingTTL refuses a binding that would outlive what the secret
// allows.
//
// The limit is the one place a credential says how long consent to it may
// last, and it has to bite where a binding is made: the lifetime arrives from
// an approval, a person's binding, or a create, and a rule enforced in one of
// those is a rule the others walk around. A secret with no limit is
// unconstrained on purpose — that is what zero says — and there the binding
// keeps whatever lifetime it was given, forever included.
func guardBindingTTL(secret *model.Secret, ttlSeconds int64) error {
	limit := secret.MaxBindingTTL
	if limit <= 0 || (ttlSeconds > 0 && ttlSeconds <= limit) {
		return nil
	}
	if ttlSeconds <= 0 {
		return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
			"a binding of secret %s may live at most %s, so it cannot be made forever; make it for less, or lift the limit with `discobox secret update %s --max-binding-ttl 0`",
			secret.Name, formatTTL(limit), secret.Name))
	}
	return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf(
		"a binding of secret %s may live at most %s, and %s was asked for; make it for less, or raise the limit with `discobox secret update %s --max-binding-ttl %d`",
		secret.Name, formatTTL(limit), formatTTL(ttlSeconds), secret.Name, ttlSeconds))
}

// defaultApprovalTTL is the lifetime of an approval that names none: what the
// request asked for, else agentcreds.DefaultGrantTTL, and never past the
// secret's limit — a default is an answer nobody chose, so it is fitted to the
// limit rather than refused by it.
func defaultApprovalTTL(req *model.SecretRequest, secret *model.Secret) int64 {
	ttl := int64(agentcreds.AskedGrantTTL(req.TTL) / time.Second)
	if ttl == 0 {
		ttl = int64(agentcreds.DefaultGrantTTL / time.Second)
	}
	if secret.MaxBindingTTL > 0 && ttl > secret.MaxBindingTTL {
		ttl = secret.MaxBindingTTL
	}
	return ttl
}

// expiryAt is when a binding of ttlSeconds made at now lapses; nil, forever,
// for zero.
func expiryAt(now time.Time, ttlSeconds int64) *time.Time {
	if ttlSeconds <= 0 {
		return nil
	}
	expires := now.Add(time.Duration(ttlSeconds) * time.Second)
	return &expires
}

// formatTTL says a lifetime the way a person reads one, so a refusal compares
// two durations rather than two integers.
func formatTTL(seconds int64) string {
	return (time.Duration(seconds) * time.Second).String()
}

// lookupSecret reads a project's secret by ID, ID prefix or name.
func (s *Service) lookupSecret(ctx context.Context, projectID, ref string) (*model.Secret, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a secret is named")
	}
	if secret, err := s.store.GetSecretByName(ctx, projectID, ref); err == nil {
		return secret, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	secret, err := s.store.GetSecret(ctx, projectID, ref)
	if err != nil {
		return nil, apperrors.NotFound(err, fmt.Sprintf("secret %s not found", ref))
	}
	return secret, nil
}

// marshalSecretValue converts a generated SecretValue to JSON plaintext for encryption.
func marshalSecretValue(val apigen.SecretValue) ([]byte, error) {
	mv := model.SecretValue{
		Token:                val.Token.Or(""),
		RefreshToken:         val.RefreshToken.Or(""),
		TokenURL:             strings.TrimSpace(val.TokenUrl.Or("")),
		ClientID:             strings.TrimSpace(val.ClientId.Or("")),
		ClientSecret:         val.ClientSecret.Or(""),
		AccessTokenExpiresAt: val.AccessTokenExpiresAt.Or(0),
		SubscriptionType:     strings.TrimSpace(val.SubscriptionType.Or("")),
		TokenRequestEncoding: string(val.TokenRequestEncoding.Or("")),
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
	return t == model.SecretTypeToken || t == model.SecretTypeOAuth || t == model.SecretTypeExchange
}

// isGenerationConflict reports whether err is the store's optimistic-concurrency
// signal, used by the OAuth refresh to detect that another writer rotated first.
func isGenerationConflict(err error) bool {
	return errors.Is(err, store.ErrGenerationConflict)
}

// secretCollision turns the uniqueness constraint into an answer. A name
// already taken is something the caller can fix; a driver's constraint text
// reaching the client as a 500 is neither readable nor true — nothing failed
// on the server's side.
func secretCollision(err error, sec *model.Secret) error {
	if err == nil || !isUniqueViolation(err) {
		return err
	}
	return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
		"this project already has a secret named %q; pick another name, or update the one it has", sec.Name))
}

// isUniqueViolation reports whether err is a uniqueness constraint refusing a
// write.
func isUniqueViolation(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "unique constraint") || strings.Contains(text, "duplicate key")
}

// sentinelFormat is the format a person asked for, and whether they asked for
// one: empty means "read it from the value", which is also what an update
// setting it to empty goes back to. A template that does not parse is refused
// here rather than stored, since the minter would quietly fall back to the
// default shape and the person would never learn their template was ignored.
func sentinelFormat(in apigen.OptString) (string, bool, error) {
	format := strings.TrimSpace(in.Or(""))
	if format == "" {
		return "", false, nil
	}
	if _, err := secretformat.ParseChosen(format); err != nil {
		return "", false, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("invalid format: %v", err))
	}
	return format, true, nil
}
