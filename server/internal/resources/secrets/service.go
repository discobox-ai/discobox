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
	"github.com/discobox-ai/discobox/pkg/bindingplan"
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
	// renewing collapses concurrent resolves of the same OAuth secret onto a single upstream renewal, so a rotating refresh token is
	// spent once.
	renewing singleflight.Group
	// renewals is, per secret ID, the value generation a rejection last forced
	// a renewal of and when, which bounds how often a refused value may spend
	// a refresh token. See renewedRecently.
	renewalsMu sync.Mutex
	renewals   map[string]spentRenewal
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

// routesFrom reads the routes a body names (ADR 26-10-27-880 §4, ADR
// 26-10-27-939). A route secretroute.Normalize refuses is refused rather than
// dropped: a person who wrote one meant somewhere.
func routesFrom(input []apigen.SecretRouteInput) ([]model.SecretRoute, error) {
	normalized, err := model.NormalizeRoutes(services.SecretRoutesToModel(input))
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
	allowRaw := input.AllowRaw.Or(false)
	if err := guardAllowRaw(ctx, allowRaw); err != nil {
		return nil, err
	}
	sec := &model.Secret{
		ProjectID:      projectID,
		Name:           name,
		Type:           secretType,
		Format:         format,
		FormatSet:      formatSet,
		MaxBindingTTL:  ttl,
		AllowRaw:       allowRaw,
		EncryptedValue: valueBytes,
	}
	sec.ValueWritten(time.Now().UTC(), nil)
	// A secret with no routes is sent nowhere (ADR 26-10-27-880 §1); `*` is how a person says anywhere.
	if routes, ok := input.Routes.Get(); ok {
		if _, err := replaceRoutes(sec, routes); err != nil {
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
	rawRevoked := false
	if allowRaw, ok := input.AllowRaw.Get(); ok {
		if err := guardAllowRaw(ctx, allowRaw); err != nil {
			return nil, err
		}
		rawRevoked = sec.AllowRaw && !allowRaw
		sec.AllowRaw = allowRaw
	}
	routesChanged := false
	if routes, ok := input.Routes.Get(); ok {
		if routesChanged, err = replaceRoutes(sec, routes); err != nil {
			return nil, err
		}
	}
	if valueVal, ok := input.Value.Get(); ok {
		valueBytes, err := marshalSecretValue(valueVal)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "invalid secret value")
		}
		sec.EncryptedValue = valueBytes
	}
	// A replaced value retracts what was recorded about the credential it
	// replaced (ADR 0132 §4). The store does it, for every writer at once —
	// this one and harness save-back's update-in-place — rather than
	// each of them remembering to.
	//
	// A running discobox's proxy holds its swaps to the routes it was last
	// handed, and keeps the value it last resolved, so a change to either
	// marks the sentinel delivery of every discobox holding the secret in the
	// same transaction: each is sent the routes as written and the value's new
	// generation, which drops the value its pool holds (#1022), and one that
	// cannot be reached now is retried.
	generation := sec.ValueGeneration()
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.UpdateSecret(ctx, sec); err != nil {
			return secretCollision(err, sec)
		}
		valueReplaced := sec.ValueGeneration() != generation
		if rawRevoked {
			if err := s.deleteRawBindingsTx(ctx, txStore, txDB, sec); err != nil {
				return err
			}
		}
		if (routesChanged || valueReplaced) && s.holders != nil {
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

// guardAllowRaw keeps letting a value be handed over raw a person's (ADR
// 26-10-27-840 §2): a discobox may not allow it, though it may take it away.
func guardAllowRaw(ctx context.Context, allowRaw bool) error {
	if principal, _ := auth.PrincipalFromContext(ctx); allowRaw && principal.Type == auth.PrincipalTypeSandbox {
		return apperrors.NewStatusError(http.StatusForbidden, "only a person allows a secret's value to be handed over raw")
	}
	return nil
}

// deleteRawBindingsTx removes the raw bindings of a secret that no longer
// allows raw (ADR 26-10-27-840 §1), each recorded as removed by whoever took
// raw away, and marks their holders, whose pools then drop the value.
func (s *Service) deleteRawBindingsTx(ctx context.Context, txStore *store.Store, txDB *gorm.DB, sec *model.Secret) error {
	bindings, err := txStore.ListSecretBindings(ctx, sec.ProjectID, store.SecretBindingFilter{SecretID: sec.ID})
	if err != nil {
		return err
	}
	change := store.BindingChange{Actor: auth.ActorFromContext(ctx), Cause: "raw no longer allowed on " + sec.ID}
	for _, binding := range bindings {
		if binding.Raw == "" {
			continue
		}
		deleted, err := txStore.DeleteSecretBinding(ctx, sec.ProjectID, binding.ID, change)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if err := s.markBindingHolders(ctx, txStore, txDB, deleted...); err != nil {
			return err
		}
	}
	return nil
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
	requests, err := s.store.ListSecretRequests(ctx, projectID, status, options...)
	if principal, _ := auth.PrincipalFromContext(ctx); principal.Type == auth.PrincipalTypeSandbox {
		for i := range requests {
			requests[i].Use.Receipt = nil
			requests[i].Use.Grant = nil
			requests[i].Delegate.Receipt = nil
			requests[i].Delegate.Grant = nil
		}
	}
	return requests, err
}

func (s *Service) GetSecretRequest(ctx context.Context, projectID, requestID string) (*model.SecretRequest, error) {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret request not found")
	}
	if principal, _ := auth.PrincipalFromContext(ctx); principal.Type == auth.PrincipalTypeSandbox {
		req.Use.Receipt = nil
		req.Use.Grant = nil
		req.Delegate.Receipt = nil
		req.Delegate.Grant = nil
	}
	return req, nil
}

// ApproveSecretRequest answers a pending request, part by part (ADR
// 26-11-02-022 §4): the answer names a decision for every part the request
// has, and no other, so a delegation is never granted by omission. An
// approved part replaces the asking discobox's own bindings of that kind for
// the secret chosen (§5, approvalWrite); a denied one changes nothing. Every
// binding, its events, the marks that deliver them and the request's decision
// are one transaction.
//
// A person may answer any request. A discobox answers only a request with no
// delegate part, from a discobox it created, and approves only under a live
// delegate binding covering it of the secret it answers with (ADR
// 26-10-27-880 §3): the binding is handed on under that one, as its child,
// and lasts no longer.
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
	if approverIsSandbox && req.Delegate.Asked {
		return nil, errDiscoboxAnswersDelegation
	}
	write := &approvalWrite{req: req, actor: principal.Actor()}
	if write.use, err = partDecision("use", &req.Use, input.Use); err != nil {
		return nil, err
	}
	if write.delegate, err = partDecision("delegate", &req.Delegate, input.Delegate); err != nil {
		return nil, err
	}
	if write.use == nil && write.delegate == nil {
		// Every part denied: nothing is bound, so no secret is chosen.
		return req, s.commitApproval(ctx, write)
	}
	secretRef := strings.TrimSpace(input.SecretId.Or(""))
	if approverIsSandbox {
		parents, secret, err := s.delegationsFor(ctx, projectID, principal.SandboxID, secretRef)
		if err != nil {
			return nil, err
		}
		write.secret, write.parents, write.approverID = secret, parents, principal.SandboxID
	} else {
		if secretRef == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "secret ID is required to approve a part")
		}
		if write.secret, err = s.lookupSecret(ctx, projectID, secretRef); err != nil {
			return nil, err
		}
	}
	if err := s.commitApproval(ctx, write); err != nil {
		return nil, err
	}
	if write.approverID != "" && write.use != nil {
		slog.InfoContext(ctx, "a discobox approved a request under its delegate binding",
			"projectId", projectID, "requestId", req.ID, "bindingId", req.Use.BindingID,
			"approver", write.approverID, "parentBindingId", write.use.binding.ParentID)
	}
	return req, nil
}

// errDiscoboxAnswersDelegation refuses a discobox answering a request with a
// delegate part, either way: handing on the power to hand on is a person's,
// and one answer is never split between two answerers (ADR 26-11-02-022 §4).
var errDiscoboxAnswersDelegation = apperrors.NewStatusError(http.StatusForbidden,
	"a request with a delegate part is a person's to answer, both parts included: handing on the power to hand on is a person's to approve")

// partDecision reads the answer's decision on one part: nil when the part is
// denied, else what approving it binds. A part the request has must be
// decided, and one it lacks must not be.
func partDecision(which string, part *model.SecretRequestPart, input apigen.OptSecretRequestPartDecision) (*partApproval, error) {
	decision, decided := input.Get()
	switch {
	case part.Asked && !decided:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the request has a %s part: the answer names a decision for it, approve or deny", which))
	case !part.Asked && decided:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the request has no %s part to decide", which))
	case !part.Asked:
		return nil, nil
	}
	switch decision.Decision {
	case apigen.SecretRequestPartDecisionDecisionDeny:
		if decision.Routes != nil || decision.TtlSeconds.Set || decision.Raw.Set || decision.As.Set || decision.Scope.Set {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("the %s part is denied, so it takes no routes, lifetime, delivery or way of granting it", which))
		}
		return nil, nil
	case apigen.SecretRequestPartDecisionDecisionApprove:
	default:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the %s part's decision is approve or deny, not %q", which, decision.Decision))
	}
	scope := decision.Scope.Or(apigen.SecretRequestPartDecisionScopeRoutes)
	approval := &partApproval{delegate: which == "delegate", routes: part.Routes, scope: string(scope)}
	switch scope {
	case apigen.SecretRequestPartDecisionScopeRoutes:
	case apigen.SecretRequestPartDecisionScopeHosts:
		hosts := secretroute.OpenRouteHosts(part.Routes)
		if len(hosts) == 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "requested hosts scope requires an open requested host")
		}
		approval.routes = []model.SecretRoute{{Hosts: hosts}}
	case apigen.SecretRequestPartDecisionScopeAnywhere:
		approval.routes = nil
	default:
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "approval scope must be routes, hosts or anywhere")
	}
	if scope != apigen.SecretRequestPartDecisionScopeRoutes && decision.Routes != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "route overrides require routes scope")
	}
	if decision.Routes != nil {
		routes, err := routesFrom(decision.Routes)
		if err != nil {
			return nil, err
		}
		approval.routes = routes
	}
	approval.ttl, approval.ttlNamed = decision.TtlSeconds.Get()
	if approval.ttlNamed && approval.ttl < 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding's lifetime is a number of seconds; 0 is forever")
	}
	// The value itself is the approver's choice, never the ask's (ADR
	// 26-10-27-840 §2), and a use's delivery: a delegation delivers nothing.
	if raw, ok := decision.Raw.Get(); ok {
		if approval.delegate {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"the value itself is the use part's delivery; a delegation delivers nothing")
		}
		approval.raw = string(raw)
	}
	// How a use part is granted is the ask's unless the approver says
	// otherwise (ADR 26-11-03-329 §4); a delegate part always replaces.
	approval.adds = part.Add
	if as, ok := decision.As.Get(); ok {
		if approval.delegate {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"a delegate part is granted as the discobox's whole delegation of the secret; only a use part may be granted as an addition")
		}
		approval.adds = as == apigen.SecretRequestPartDecisionAsAdd
	}
	if approval.adds && approval.raw != "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"an addition is delivered by sentinel; grant the value itself as a replacement")
	}
	return approval, nil
}

// approvalWrite is one answer to a request, as its transaction makes it. use
// and delegate are the approved parts; nil is a part denied or not asked.
type approvalWrite struct {
	req           *model.SecretRequest
	secret        *model.Secret
	use, delegate *partApproval
	actor         string
	// parents are, for a discobox's approval, the delegate bindings it may
	// hand the secret on under, longest-lived first; approverID is the
	// discobox.
	parents    []model.SecretBinding
	approverID string
}

// partApproval is one part approved: the routes and lifetime the approver
// chose, and for the use part the delivery. binding is what the approval
// made or replaced.
type partApproval struct {
	scope    string
	delegate bool
	routes   []model.SecretRoute
	// raw is the delivery the approver chose for the value itself, empty for
	// a sentinel (ADR 26-10-27-840 §2).
	raw string
	// ttl is the lifetime the approver named, read only when ttlNamed.
	ttl      int64
	ttlNamed bool
	// adds grants a use part as an addition beside what the discobox
	// holds, replacing nothing (ADR 26-11-03-329 §4).
	adds    bool
	binding *model.SecretBinding
}

// commitApproval makes every write an answer makes as one act: each approved
// part's binding, replacing the discobox's own of that kind, with their
// events, the marks that deliver them, and the request decided part by part.
// A refusal anywhere — a variable already holding another credential, a
// binding it may not take over, a delegate binding gone, the request answered
// concurrently — leaves none of them behind.
func (s *Service) commitApproval(ctx context.Context, a *approvalWrite) error {
	req, projectID := a.req, a.req.ProjectID
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		now := time.Now().UTC()
		if principal, _ := auth.PrincipalFromContext(ctx); principal.Type == auth.PrincipalTypeSandbox {
			if err := lockHandoff(ctx, txStore, projectID, principal.SandboxID, req.SandboxID); err != nil {
				return err
			}
		}
		var secret *model.Secret
		if a.use != nil || a.delegate != nil {
			var err error
			if secret, err = txStore.GetSecret(ctx, projectID, a.secret.ID); err != nil {
				return apperrors.NotFound(err, "secret not found")
			}
		}
		change := store.BindingChange{Actor: a.actor, Cause: req.ID}
		for _, part := range []struct {
			approval *partApproval
			asked    *model.SecretRequestPart
		}{{a.use, &req.Use}, {a.delegate, &req.Delegate}} {
			if !part.asked.Asked {
				continue
			}
			if part.approval == nil {
				part.asked.Outcome = model.SecretRequestPartDenied
				continue
			}
			binding, err := a.binding(ctx, txStore, secret, part.approval, now)
			if err != nil {
				return err
			}
			var made *model.SecretBinding
			if part.approval.adds {
				made, err = s.addBinding(ctx, txStore, txDB, binding, now, change)
			} else {
				made, err = s.replaceBindings(ctx, txStore, txDB, binding, part.approval.raw, change)
			}
			if err != nil {
				return err
			}
			part.approval.binding = made
			part.asked.Outcome = model.SecretRequestPartApproved
			part.asked.BindingID = made.ID
			part.asked.ApprovedScope = part.approval.scope
			grant, err := approvalGrant(ctx, txStore, secret, made)
			if err != nil {
				return err
			}
			part.asked.Grant = grant

			part.asked.Receipt = made.Receipt
			if !part.approval.delegate {
				part.asked.ApprovedAs = model.SecretRequestApprovedAsReplace
				if part.approval.adds {
					part.asked.ApprovedAs = model.SecretRequestApprovedAsAdd
				}
			}
			req.SecretID = secret.ID
		}
		req.Status = model.SecretRequestStatusDenied
		if req.Use.Outcome == model.SecretRequestPartApproved || req.Delegate.Outcome == model.SecretRequestPartApproved {
			req.Status = model.SecretRequestStatusApproved
		}
		req.DecidedBy = a.actor
		decidedAt := time.Now().UTC()
		req.DecidedAt = &decidedAt
		return txStore.UpdateSecretRequestIfPending(ctx, req)
	})
	if errors.Is(err, store.ErrGenerationConflict) {
		return apperrors.NewStatusError(http.StatusConflict, "secret request status changed concurrently; refresh and try again")
	}
	return err
}

// approvalGrant snapshots the independent authorization lists, never the value.
func approvalGrant(ctx context.Context, st *store.Store, secret *model.Secret, binding *model.SecretBinding) (*model.SecretRequestGrant, error) {
	grant := &model.SecretRequestGrant{SecretID: secret.ID, SecretRevision: secret.Revision,
		ParentBindingID: binding.ParentID, ExpiresAt: binding.ExpiresAt, Delivery: "sentinel", Mode: "replace"}
	if binding.Delegate {
		grant.Delivery = "none"
	}
	if binding.Raw != "" {
		grant.Delivery = binding.Raw
	}
	if binding.Adds {
		grant.Mode = "add"
	}
	grant.RouteLists = append(grant.RouteLists, model.SecretRequestGrantRouteList{Source: "secret", SourceID: secret.ID, Routes: secret.Routes})
	for _, routes := range secret.RouteConstraints {
		grant.RouteLists = append(grant.RouteLists, model.SecretRequestGrantRouteList{Source: "secret-constraint", SourceID: secret.ID, Routes: routes})
	}
	grant.RouteLists = append(grant.RouteLists, model.SecretRequestGrantRouteList{Source: "binding", SourceID: binding.ID, Routes: binding.Routes})
	if binding.ParentID != "" {
		parent, err := st.GetSecretBinding(ctx, binding.ProjectID, binding.ParentID)
		if err != nil {
			return nil, err
		}
		grant.RouteLists = append(grant.RouteLists, model.SecretRequestGrantRouteList{Source: "parent", SourceID: parent.ID, Routes: parent.Routes})
	}
	// Empty restrictions are meaningful and must encode as arrays, not null.
	for i := range grant.RouteLists {
		if grant.RouteLists[i].Routes == nil {
			grant.RouteLists[i].Routes = []model.SecretRoute{}
		}
	}
	return grant, nil
}

// binding is the binding approving one part asks for: on the discobox that
// asked, of the secret chosen, for that part's kind, with the lifetime the
// approver named — else the ask's — fitted, for a discobox approving, to the
// delegate binding it hands it on under.
func (a *approvalWrite) binding(ctx context.Context, txStore *store.Store, secret *model.Secret, part *partApproval, now time.Time) (*model.SecretBinding, error) {
	ttl := part.ttl
	if !part.ttlNamed {
		ttl = defaultApprovalTTL(a.req, secret)
	}
	binding := &model.SecretBinding{
		ProjectID:  a.req.ProjectID,
		SecretID:   secret.ID,
		TargetKind: model.SecretBindingTargetSandbox,
		TargetID:   a.req.SandboxID,
		Use:        !part.delegate,
		Delegate:   part.delegate,
		Routes:     part.routes,
		Raw:        part.raw,
		Adds:       part.adds,
		RequestID:  a.req.ID,
		CreatedBy:  a.actor,
	}
	if binding.Use {
		binding.EnvName = a.req.EnvName
	}
	if a.approverID != "" {
		if err := binding.BeforeCreate(nil); err != nil {
			return nil, err
		}
		current, fitted, err := fitDelegation(ctx, txStore, a.approverID, binding, a.parents, ttl, part.ttlNamed)
		if err != nil {
			return nil, err
		}
		ttl = fitted
		*secret = *current
	}
	if err := guardBindingTTL(secret, ttl); err != nil {
		return nil, err
	}
	if err := checkRawBinding(ctx, binding, secret); err != nil {
		return nil, err
	}
	if a.approverID == "" {
		if err := checkSecretBodyPlaces(binding, secret, nil); err != nil {
			return nil, err
		}
		binding.ExpiresAt = expiryAt(now, ttl)
	}
	if err := checkBindingShape(binding); err != nil {
		return nil, err
	}
	return binding, nil
}

// addBinding stores want, an addition, beside the discobox's bindings and
// touches none of them (ADR 26-11-03-329 §4): it stacks on the binding that
// delivers its variable. A variable delivered by a binding of another secret
// is refused, as a replacement refuses it, since the addition would never
// apply there. So is one whose binding hands over the value itself: the
// proxy holds that value to its sentinel's grants, so an addition would widen
// where the value goes on a sentinel ask nobody read as one (ADR
// 26-11-03-329 Deferred; checkReplaced's raw refusal).
func (s *Service) addBinding(ctx context.Context, txStore *store.Store, txDB *gorm.DB, want *model.SecretBinding, now time.Time, change store.BindingChange) (*model.SecretBinding, error) {
	sandbox, err := txStore.GetSandbox(ctx, want.ProjectID, want.TargetID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	effective, err := txStore.ResolveSandboxBindings(ctx, sandbox, now)
	if err != nil {
		return nil, err
	}
	if applied, ok := effective.UseFor(want.EnvName); ok {
		if applied.Binding.SecretID != want.SecretID {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"%s already holds another credential in this discobox, so an addition of this one would never apply there; approve with that secret, or ask for another variable", want.EnvName))
		}
		if applied.Binding.Raw != "" {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"%s holds this credential's value itself in this discobox (binding %s), and an addition would widen where that value may go; grant the value again as a replacement, or change the binding", want.EnvName, applied.Binding.ID))
		}
	}
	if err := txStore.CreateSecretBinding(ctx, want, change); err != nil {
		return nil, bindingConflict(err)
	}
	return want, s.markBindingHolders(ctx, txStore, txDB, *want)
}

// replaceBindings makes want the discobox's one sandbox-level binding of its
// kind for its secret (ADR 26-11-02-022 §5), and returns it as stored. A
// request states the whole access of that kind, so approving it never adds to
// what stands:
//
//   - Use: of the discobox's use bindings of the secret, the use-only one in
//     the asked variable — else the oldest — is updated in place; the other
//     use-only ones are deleted, their sentinels with them; a binding that
//     also delegates loses only its use side and is never deleted, so its
//     delegation and everything handed on under it stay. With no use-only
//     binding left to update, want is made, and takes over the variable — and
//     so the sentinel — a split binding delivered it in.
//   - Delegate: the delegate binding of the secret is updated in place,
//     keeping its ID, so what was handed on under it stays and is held to its
//     new routes. One that also uses keeps its ID as the delegate binding, and
//     its use side splits off into a new use binding in its variable, with the
//     routes and lifetime the use side had.
//
// Bindings on the discobox's harness config and project are not its own and
// are not touched. Every binding this would update, split or delete is
// checked first (checkReplaced); any refusal refuses the whole approval.
func (s *Service) replaceBindings(ctx context.Context, txStore *store.Store, txDB *gorm.DB, want *model.SecretBinding, raw string, change store.BindingChange) (*model.SecretBinding, error) {
	standing, err := txStore.ListSecretBindings(ctx, want.ProjectID, store.SecretBindingFilter{TargetKind: want.TargetKind, TargetID: want.TargetID})
	if err != nil {
		return nil, err
	}
	// Which standing binding is kept, which go and which lose a side is
	// bindingplan's rule, the one the CLI and the console show before a
	// person approves; every binding it touches is checked here first. The
	// use part is applied before the delegate part, each on the bindings as
	// they then stand, which is bindingplan.Answer.
	byID := map[string]*model.SecretBinding{}
	var mine []bindingplan.Binding
	for i := range standing {
		binding := &standing[i]
		if want.Use && binding.Use && binding.SecretID != want.SecretID && binding.EnvName == want.EnvName {
			return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
				"%s already holds another credential in this discobox; approve the request with that secret, or ask for another variable", want.EnvName))
		}
		if binding.SecretID != want.SecretID {
			continue
		}
		byID[binding.ID] = binding
		mine = append(mine, planBinding(binding))
	}
	var plan bindingplan.Plan
	if want.Use {
		plan = bindingplan.Use(mine, want.EnvName)
	} else {
		plan = bindingplan.Delegate(mine)
	}
	for _, step := range plan.Steps {
		if err := checkReplaced(byID[step.Binding.ID], want, raw); err != nil {
			return nil, err
		}
	}
	var touched []model.SecretBinding
	// Clear and delete first, so the variable want takes is free.
	for _, step := range plan.Steps {
		binding := byID[step.Binding.ID]
		switch step.Fate {
		case bindingplan.LosesUse:
			before := *binding
			binding.Use, binding.EnvName, binding.Raw = false, "", ""
			if err := txStore.UpdateSecretBinding(ctx, binding, change); err != nil {
				return nil, bindingConflict(err)
			}
			touched = append(touched, before)
		case bindingplan.Deleted:
			deleted, err := txStore.DeleteSecretBinding(ctx, binding.ProjectID, binding.ID, change)
			if err != nil {
				return nil, err
			}
			touched = append(touched, deleted...)
		}
	}
	if keep := byID[plan.Keep]; keep != nil && want.Use {
		before := *keep
		keep.EnvName, keep.Routes, keep.ExpiresAt, keep.Raw = want.EnvName, want.Routes, want.ExpiresAt, want.Raw
		keep.Receipt = want.Receipt
		if keep.Receipt != nil {
			keep.Receipt.BindingID = keep.ID
		}
		if err := txStore.UpdateSecretBinding(ctx, keep, change); err != nil {
			return nil, bindingConflict(err)
		}
		return keep, s.markBindingHolders(ctx, txStore, txDB, append(touched, before, *keep)...)
	} else if keep != nil {
		before := *keep
		if before.Use {
			// The split: the standing binding stays the delegation, and
			// its use side becomes a binding of its own in its variable.
			keep.Use, keep.EnvName, keep.Raw = false, "", ""
		}
		keep.Routes, keep.ExpiresAt = want.Routes, want.ExpiresAt
		if err := txStore.UpdateSecretBinding(ctx, keep, change); err != nil {
			return nil, bindingConflict(err)
		}
		touched = append(touched, before, *keep)
		if before.Use {
			split := &model.SecretBinding{
				ProjectID:  before.ProjectID,
				SecretID:   before.SecretID,
				TargetKind: before.TargetKind,
				TargetID:   before.TargetID,
				Use:        true,
				EnvName:    before.EnvName,
				Routes:     before.Routes,
				ExpiresAt:  before.ExpiresAt,
				ParentID:   before.ParentID,
				RequestID:  before.RequestID,
				CreatedBy:  before.CreatedBy,
			}
			if err := txStore.CreateSecretBinding(ctx, split, change); err != nil {
				return nil, bindingConflict(err)
			}
			touched = append(touched, *split)
		}
		return keep, s.markBindingHolders(ctx, txStore, txDB, touched...)
	}
	if err := txStore.CreateSecretBinding(ctx, want, change); err != nil {
		return nil, bindingConflict(err)
	}
	return want, s.markBindingHolders(ctx, txStore, txDB, append(touched, *want)...)
}

// checkReplaced refuses an approval that would update, split or delete a
// binding it may not (ADR 26-11-02-022 §5): one held under another parent —
// a person's, or one handed on under another delegation — is not taken over;
// one holding the value itself is renewed or removed only by a grant of the
// value again, as a person chose it (ADR 26-10-27-840 §2), and one that also
// delegates is not split at all.
func checkReplaced(standing, want *model.SecretBinding, raw string) error {
	if standing.ParentID != want.ParentID {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"this discobox already holds that credential under another binding (%s); remove it first", standing.ID))
	}
	if standing.Raw == "" {
		return nil
	}
	if standing.Use && standing.Delegate {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"binding %s holds this credential's value itself and also delegates it; change that binding (admin binding update) rather than approving over it", standing.ID))
	}
	if standing.Raw != raw {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf(
			"%s holds this credential's value itself in this discobox (binding %s); renew it by granting the value again (raw %s), or on the binding", standing.EnvName, standing.ID, standing.Raw))
	}
	return nil
}

// markBindingHolders marks the discoboxes bindings cover, when the service
// knows what marks them.
func (s *Service) markBindingHolders(ctx context.Context, txStore *store.Store, txDB *gorm.DB, bindings ...model.SecretBinding) error {
	if s.holders == nil || len(bindings) == 0 {
		return nil
	}
	return s.holders.MarkBindingHoldersDirtyTx(ctx, txStore, txDB, bindings[0].ProjectID, bindings)
}

// DenySecretRequest denies every part of a pending request. A discobox
// denies only a request with no delegate part, as it approves only one: a
// request that has one is a person's to answer (ADR 26-11-02-022 §4).
func (s *Service) DenySecretRequest(ctx context.Context, projectID, requestID string) error {
	req, err := s.store.GetSecretRequest(ctx, projectID, requestID)
	if err != nil {
		return apperrors.NotFound(err, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusPending {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("secret request is already %s", req.Status))
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Type == auth.PrincipalTypeSandbox && req.Delegate.Asked {
		return errDiscoboxAnswersDelegation
	}
	return s.commitApproval(ctx, &approvalWrite{req: req, actor: principal.Actor()})
}

// ResolveSandboxSecret resolves a sentinel injected into a sandbox. The
// sentinel names the binding that delivered it; the value is handed out only
// while that binding stands — live, of this secret, on a level covering the
// sandbox, and under a live parent when it was handed on — and only for a
// host some grant the sentinel is held to allows, in every one of that
// grant's route lists (ADR 26-10-27-880 §4, ADR 26-11-03-329 §3): the
// binding's own, or an addition's stacking on it. The pool may hold the
// value until the latest of the grants allowing the host lapses. Anything
// else is refused: no request is opened, since a sentinel exists only because
// a binding does.
func (s *Service) ResolveSandboxSecret(ctx context.Context, poolID, sandboxID, sentinel, host string) (*model.SandboxSecretResolution, error) {
	r, err := s.resolution(ctx, poolID, sandboxID, sentinel)
	if err != nil {
		return nil, err
	}
	// A raw binding's sentinel is the pool's handle for the value, never
	// swapped: its value is handed out only by ResolveSandboxRawSecret.
	if !r.ok || r.assignment.Raw != "" || r.binding.Raw != "" {
		return deniedResolution(), nil
	}
	now := time.Now().UTC()
	grants, err := s.store.SentinelGrantsFor(ctx, r.sandbox, r.assignment, now)
	if err != nil {
		return nil, err
	}
	allowing := secretroute.GrantsAllowingHost(grants, now, normalizeHost(host))
	if len(allowing) == 0 {
		return deniedResolution(), nil
	}
	r.expiresAt = secretroute.LatestExpiry(allowing)
	out, err := s.handOut(ctx, r)
	if err != nil {
		return nil, err
	}
	if out.ReportScope, err = s.reportScope(ctx, r.sandbox, r.assignment.EnvName); err != nil {
		return nil, err
	}
	return out, nil
}

// ResolveSandboxRawSecret hands a sandbox's pool the value of one of its raw
// bindings (ADR 26-10-27-840 §3), by the sentinel that is its handle: to hand
// the sandbox, and to watch its requests for. It is the second door cleartext
// leaves by, and the only one with no host: the value goes to the sandbox
// itself. It answers only for a row and a binding that are raw, of a secret
// that still allows raw, from a binding that stands, and records the
// binding's delivered event in the same transaction that reads the row and
// the binding again: one removed or made a sentinel in between is denied, and
// the pool drops the value.
func (s *Service) ResolveSandboxRawSecret(ctx context.Context, poolID, sandboxID, sentinel string) (*model.SandboxSecretResolution, error) {
	r, err := s.resolution(ctx, poolID, sandboxID, sentinel)
	if err != nil {
		return nil, err
	}
	if !r.ok || r.assignment.Raw == "" || r.binding.Raw != r.assignment.Raw || !r.secret.AllowRaw || r.parent != nil {
		return deniedResolution(), nil
	}
	out, err := s.handOut(ctx, r)
	if err != nil {
		return nil, err
	}
	if out.Value == nil || out.Value.Token == "" {
		return deniedResolution(), nil
	}
	if err := s.store.RecordRawDelivery(ctx, r.assignment, r.binding.ID, out.Revision); errors.Is(err, store.ErrNotFound) {
		return deniedResolution(), nil
	} else if err != nil {
		return nil, err
	}
	out.BindingID = r.binding.ID
	return out, nil
}

// resolved is what a pool's resolve of a sentinel stands on, read once for
// both doors: the row, its sandbox, the secret and the binding it was
// delivered under. ok is false when that binding no longer stands.
type resolved struct {
	assignment *model.SandboxSecret
	sandbox    *model.Sandbox
	secret     *model.Secret
	binding    *model.SecretBinding
	parent     *model.SecretBinding
	ok         bool
	// expiresAt is how long the value may be held for what it resolves:
	// the binding's expiry, or for a swap the latest of the grants allowing
	// its host (ADR 26-11-03-329 §3).
	expiresAt *time.Time
}

func deniedResolution() *model.SandboxSecretResolution {
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusDenied}
}

// resolution reads what a resolve of sentinel stands on, refusing a sandbox
// the calling pool does not hold.
func (s *Service) resolution(ctx context.Context, poolID, sandboxID, sentinel string) (resolved, error) {
	assignment, err := s.store.GetSandboxSecretBySentinel(ctx, sandboxID, sentinel)
	if err != nil {
		return resolved{}, apperrors.NotFound(err, "sandbox secret not found")
	}
	// The calling pool agent must own the sandbox the sentinel belongs to.
	sandbox, err := s.store.GetSandbox(ctx, assignment.ProjectID, assignment.SandboxID)
	if err != nil {
		return resolved{}, apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return resolved{}, apperrors.NewStatusError(http.StatusNotFound, "sandbox secret not found")
	}
	secret, err := s.store.GetSecret(ctx, assignment.ProjectID, assignment.SecretID)
	if err != nil {
		return resolved{}, apperrors.NotFound(err, "secret not found")
	}
	binding, parent, ok, err := s.standingBinding(ctx, sandbox, assignment, time.Now().UTC())
	if err != nil {
		return resolved{}, err
	}
	r := resolved{assignment: assignment, sandbox: sandbox, secret: secret, binding: binding, parent: parent, ok: ok}
	if ok {
		r.expiresAt = store.BindingExpiry(binding, parent)
	}
	return r, nil
}

// handOut is the value a standing binding resolves to, renewed first when it
// renews, with how long the pool may hold it: r.expiresAt, capped by the
// token's own and by its staleness. Revision is the secret's revision the
// value is of.
func (s *Service) handOut(ctx context.Context, r resolved) (*model.SandboxSecretResolution, error) {
	secret := r.secret
	val, err := s.store.OpenSecretValue(ctx, secret)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	expiresAt := r.expiresAt
	if renews(secret.Type) {
		// Renew a near-expired token before handing it out, and cap the
		// cache lifetime the proxy honors by the token's own expiry so it
		// re-resolves — and renews again — as the token ages out.
		renewed, err := s.ensureFresh(ctx, secret, val)
		if err != nil {
			return nil, err
		}
		if renewed != val {
			// Renewed under its own read of the secret: the revision the
			// value is of is the one it wrote.
			if current, err := s.store.GetSecret(ctx, secret.ProjectID, secret.ID); err == nil {
				secret = current
			}
		}
		val = renewed
		expiresAt = tokenResolutionExpiry(expiresAt, val)
	}
	if secret.Renewable() {
		expiresAt = s.renewableResolution(ctx, secret, r.sandbox.ID, expiresAt)
	}
	return &model.SandboxSecretResolution{Status: model.SecretRequestStatusApproved, Value: val, ExpiresAt: expiresAt, SecretID: secret.ID, Revision: secret.Revision, ValueGeneration: secret.ValueGeneration()}, nil
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
	return t == model.SecretTypeToken || t == model.SecretTypeOAuth
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

// planBinding is a binding as bindingplan reads it.
func planBinding(binding *model.SecretBinding) bindingplan.Binding {
	return bindingplan.Binding{ID: binding.ID, Use: binding.Use, Delegate: binding.Delegate, Adds: binding.Adds, EnvName: binding.EnvName, Created: binding.CreatedAt}
}
