package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Bindings are the authorization (ADR 26-10-27-880 §1). A person makes,
// changes and removes them here; an approval (ApproveSecretRequest) and a
// create (PrepareSandboxBindings) make them too. Every write marks the
// discoboxes the binding's target covers in its transaction, so each is
// materialized again and its pool sent the result.

// refuseSandboxPrincipal keeps the binding routes a person's: a discobox
// hands a credential on only through an approval or a create, under a
// delegate binding, never by writing a binding.
func refuseSandboxPrincipal(ctx context.Context) error {
	if principal, ok := auth.PrincipalFromContext(ctx); ok && principal.Type == auth.PrincipalTypeSandbox {
		return apperrors.NewStatusError(http.StatusForbidden, "bindings are a person's to make, change and remove")
	}
	return nil
}

// ListSecretBindings lists a project's bindings, live or not.
func (s *Service) ListSecretBindings(ctx context.Context, projectID string, query services.SecretBindingQuery) ([]model.SecretBinding, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	filter := store.SecretBindingFilter{TargetKind: query.TargetKind, ParentID: strings.TrimSpace(query.ParentID)}
	if ref := strings.TrimSpace(query.Secret); ref != "" {
		secret, err := s.lookupSecret(ctx, projectID, ref)
		if err != nil {
			return nil, err
		}
		filter.SecretID = secret.ID
	}
	if ref := strings.TrimSpace(query.Target); ref != "" || query.TargetKind == model.SecretBindingTargetProject {
		if query.TargetKind == "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a target is named with its kind")
		}
		targetID, err := s.lookupTarget(ctx, projectID, query.TargetKind, ref)
		if err != nil {
			return nil, err
		}
		filter.TargetID = targetID
	}
	bindings, err := s.store.ListSecretBindings(ctx, projectID, filter)
	if err != nil {
		return nil, err
	}
	s.nameBindings(ctx, projectID, bindings)
	return bindings, nil
}

// GetSecretBinding reads one binding by ID or ID prefix.
func (s *Service) GetSecretBinding(ctx context.Context, projectID, bindingID string) (*model.SecretBinding, error) {
	binding, err := s.store.GetSecretBinding(ctx, projectID, bindingID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret binding not found")
	}
	if secret, err := s.store.GetSecret(ctx, projectID, binding.SecretID); err == nil {
		binding.SecretName = secret.Name
	}
	return binding, nil
}

// CreateSecretBinding binds a secret at a target: to use it in a variable,
// to delegate it, or both. Its lifetime defaults to the secret's limit and is
// held to it.
func (s *Service) CreateSecretBinding(ctx context.Context, projectID string, input services.CreateSecretBindingBody) (*model.SecretBinding, error) {
	if err := refuseSandboxPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	secret, err := s.lookupSecret(ctx, projectID, input.Secret)
	if err != nil {
		return nil, err
	}
	kind := string(input.TargetKind)
	targetID, err := s.lookupTarget(ctx, projectID, kind, input.Target.Or(""))
	if err != nil {
		return nil, err
	}
	delegate := input.Delegate.Or(false)
	use := input.Use.Or(!delegate)
	routes, err := routesFrom(input.Routes)
	if err != nil {
		return nil, err
	}
	binding := &model.SecretBinding{
		ProjectID:  projectID,
		SecretID:   secret.ID,
		SecretName: secret.Name,
		TargetKind: kind,
		TargetID:   targetID,
		Use:        use,
		Delegate:   delegate,
		EnvName:    strings.TrimSpace(input.EnvVar.Or("")),
		Routes:     routes,
		Raw:        string(input.Raw.Or("")),
	}
	if err := checkBindingShape(binding); err != nil {
		return nil, err
	}
	if err := checkRawBinding(ctx, binding, secret); err != nil {
		return nil, err
	}
	ttl, named := input.TtlSeconds.Get()
	if !named {
		ttl = secret.MaxBindingTTL
	}
	if ttl < 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding's lifetime is a number of seconds; 0 is forever")
	}
	if err := guardBindingTTL(secret, ttl); err != nil {
		return nil, err
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	binding.CreatedBy = principal.Actor()
	binding.ExpiresAt = expiryAt(time.Now().UTC(), ttl)
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		if err := txStore.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: binding.CreatedBy}); err != nil {
			return bindingConflict(err)
		}
		return s.markBindingHolders(ctx, txStore, txDB, *binding)
	}); err != nil {
		return nil, err
	}
	return binding, nil
}

// checkBindingShape refuses a binding that does not hold together: one that
// does nothing, a use with no valid variable, or a delegation alone naming
// one.
func checkBindingShape(binding *model.SecretBinding) error {
	switch {
	case !binding.Use && !binding.Delegate:
		return apperrors.NewStatusError(http.StatusBadRequest, "a binding is for use, for delegation, or both")
	case binding.Use && !services.EnvVarNamePattern.MatchString(binding.EnvName):
		return apperrors.NewStatusError(http.StatusBadRequest, "a binding for use names the environment variable the credential is delivered in")
	case !binding.Use && binding.EnvName != "":
		return apperrors.NewStatusError(http.StatusBadRequest, "a binding for delegation alone delivers nothing, so it names no environment variable")
	}
	return nil
}

// checkRawBinding refuses a raw binding that may not be (ADR 26-10-27-840
// §§1-2): one with no valid delivery, one not for use, one of a secret that
// does not allow raw, one handed on under a delegation, and one a discobox
// makes. A sentinel binding passes.
func checkRawBinding(ctx context.Context, binding *model.SecretBinding, secret *model.Secret) error {
	if binding.Raw == "" {
		return nil
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	switch {
	case !sandboxconfig.ValidRawDelivery(binding.Raw):
		return apperrors.NewStatusError(http.StatusBadRequest, "a raw binding delivers the value in its variable (env) or in a file whose path is in it (file)")
	case !binding.Use:
		return apperrors.NewStatusError(http.StatusBadRequest, "a raw binding hands the value over to use; a delegation alone delivers nothing")
	case !secret.AllowRaw:
		return apperrors.NewStatusError(http.StatusForbidden,
			fmt.Sprintf("secret %s does not allow its value to be handed over raw; a person allows it on the secret first", secret.Name))
	case binding.ParentID != "" || principal.Type == auth.PrincipalTypeSandbox:
		return apperrors.NewStatusError(http.StatusForbidden,
			"only a person hands a value over raw: a discobox hands on a sentinel, which can be taken back")
	}
	return nil
}

// UpdateSecretBinding changes a binding's routes, lifetime, variable, or what
// it is for. A lifetime named runs from now, held to the secret's limit and,
// for a binding handed on, to its parent's. Turning use off takes the variable
// away, and its sentinel with it; turning use on needs a free variable.
// A delivery named makes it raw or a sentinel again (checkRawBinding); use
// off makes it a sentinel. Turning delegate off removes every binding handed
// on under this one, each
// with its event; turning it on is refused for a binding handed on itself,
// since no chain grows from a discobox (ADR 26-10-27-880 §3). A binding left
// for neither is refused: remove it instead.
func (s *Service) UpdateSecretBinding(ctx context.Context, projectID, bindingID string, input services.UpdateSecretBindingBody) (*model.SecretBinding, error) {
	if err := refuseSandboxPrincipal(ctx); err != nil {
		return nil, err
	}
	binding, err := s.store.GetSecretBinding(ctx, projectID, bindingID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret binding not found")
	}
	secret, err := s.store.GetSecret(ctx, projectID, binding.SecretID)
	if err != nil {
		return nil, apperrors.NotFound(err, "secret not found")
	}
	previous := *binding
	if input.Routes != nil {
		if binding.Routes, err = routesFrom(input.Routes); err != nil {
			return nil, err
		}
	}
	if use, ok := input.Use.Get(); ok {
		binding.Use = use
		if !use {
			binding.EnvName = ""
			binding.Raw = ""
		}
	}
	if raw, ok := input.Raw.Get(); ok {
		binding.Raw = string(raw)
		if raw == apigen.UpdateSecretBindingBodyRawNone {
			binding.Raw = ""
		}
	}
	if env, ok := input.EnvVar.Get(); ok {
		binding.EnvName = strings.TrimSpace(env)
	}
	if delegate, ok := input.Delegate.Get(); ok {
		if delegate && !binding.Delegate && binding.ParentID != "" {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"a binding handed on under a delegation may not delegate in turn: no chain grows from a discobox")
		}
		binding.Delegate = delegate
	}
	if !binding.Use && !binding.Delegate {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a binding for neither use nor delegation does nothing; remove it instead")
	}
	if err := checkBindingShape(binding); err != nil {
		return nil, err
	}
	if err := checkRawBinding(ctx, binding, secret); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if ttl, ok := input.TtlSeconds.Get(); ok {
		if ttl < 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "a binding's lifetime is a number of seconds; 0 is forever")
		}
		if err := guardBindingTTL(secret, ttl); err != nil {
			return nil, err
		}
		if binding.ParentID != "" {
			parent, err := s.store.SecretBindingByExactID(ctx, projectID, binding.ParentID)
			if err != nil {
				return nil, apperrors.NotFound(err, "the delegate binding this was handed on under is gone")
			}
			if _, err := fitToParent(parent, ttl, true, now); err != nil {
				return nil, err
			}
		}
		binding.ExpiresAt = expiryAt(now, ttl)
	}
	change := store.BindingChange{Actor: auth.ActorFromContext(ctx)}
	if err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		// A child's sentinel is held to its parent's routes as one of its
		// lists, so a change to a parent reaches its children's holders too.
		// A shortened parent shortens nothing on its own: its children keep
		// their expiry, and lapse with it when it lapses (ResolveBindings).
		children, err := txStore.ListSecretBindings(ctx, projectID, store.SecretBindingFilter{ParentID: binding.ID})
		if err != nil {
			return err
		}
		if err := txStore.UpdateSecretBinding(ctx, binding, change); err != nil {
			return bindingConflict(apperrors.NotFound(err, "secret binding not found"))
		}
		if previous.Delegate && !binding.Delegate {
			if _, err := txStore.DeleteChildBindings(ctx, projectID, binding.ID); err != nil {
				return err
			}
		}
		return s.markBindingHolders(ctx, txStore, txDB, append([]model.SecretBinding{previous, *binding}, children...)...)
	}); err != nil {
		return nil, err
	}
	binding.SecretName = secret.Name
	return binding, nil
}

// DeleteSecretBinding removes a binding and every binding handed on under it
// (ADR 26-10-27-880 §3), returning their IDs, the one asked for first.
func (s *Service) DeleteSecretBinding(ctx context.Context, projectID, bindingID string) ([]string, error) {
	if err := refuseSandboxPrincipal(ctx); err != nil {
		return nil, err
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	var ids []string
	err := s.store.Transaction(ctx, func(txStore *store.Store, txDB *gorm.DB) error {
		deleted, err := txStore.DeleteSecretBinding(ctx, projectID, bindingID, store.BindingChange{Actor: principal.Actor()})
		if err != nil {
			return apperrors.NotFound(err, "secret binding not found")
		}
		for _, binding := range deleted {
			ids = append(ids, binding.ID)
		}
		return s.markBindingHolders(ctx, txStore, txDB, deleted...)
	})
	return ids, err
}

// ListSecretBindingEvents reads a project's binding history, newest first.
func (s *Service) ListSecretBindingEvents(ctx context.Context, projectID string, query services.SecretBindingEventQuery) ([]model.SecretBindingEvent, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	filter := store.SecretBindingEventFilter{BindingID: strings.TrimSpace(query.BindingID), Since: query.Since, Limit: query.Limit}
	if ref := strings.TrimSpace(query.Secret); ref != "" {
		secret, err := s.lookupSecret(ctx, projectID, ref)
		if err != nil {
			return nil, err
		}
		filter.SecretID = secret.ID
	}
	events, err := s.store.ListSecretBindingEvents(ctx, projectID, filter)
	if err != nil {
		return nil, err
	}
	// An event written before its snapshot carried the secret's name takes
	// the name it has now.
	names := map[string]string{}
	for i := range events {
		if events[i].Binding.SecretName != "" {
			continue
		}
		name, ok := names[events[i].SecretID]
		if !ok {
			if secret, err := s.store.GetSecret(ctx, projectID, events[i].SecretID); err == nil {
				name = secret.Name
			}
			names[events[i].SecretID] = name
		}
		events[i].Binding.SecretName = name
	}
	return events, nil
}

// ListBoundSecrets resolves what applies at a target (ADR 26-10-27-880 §2):
// per variable the use binding on the narrowest level that has one, and every
// live delegate binding, each with the route lists a request carrying it must
// pass.
func (s *Service) ListBoundSecrets(ctx context.Context, projectID, targetKind, target string) ([]services.BoundSecret, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	targetID, err := s.lookupTarget(ctx, projectID, targetKind, target)
	if err != nil {
		return nil, err
	}
	targets := store.BindingTargetsAt(projectID, targetKind, targetID)
	if targetKind == model.SecretBindingTargetSandbox {
		sandbox, err := s.store.GetSandbox(ctx, projectID, targetID)
		if err != nil {
			return nil, apperrors.NotFound(err, "sandbox not found")
		}
		targets = store.SandboxBindingTargets(sandbox)
	}
	effective, err := s.store.ResolveBindings(ctx, projectID, targets, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	var out []services.BoundSecret
	seen := map[string]bool{}
	for _, applied := range append(effective.Use, effective.Delegate...) {
		if seen[applied.Binding.ID] {
			continue
		}
		seen[applied.Binding.ID] = true
		secret, err := s.store.GetSecret(ctx, projectID, applied.Binding.SecretID)
		if err != nil {
			continue
		}
		out = append(out, services.BoundSecret{Binding: applied.Binding, SecretName: secret.Name,
			RouteLists: store.RouteLists(secret, &applied.Binding, applied.Parent),
			ExpiresAt:  bindingExpiry(&applied.Binding, applied.Parent)})
	}
	return out, nil
}

// lookupTarget resolves a binding target named by name or ID to its ID. A
// project target is the project itself, and names nothing else.
func (s *Service) lookupTarget(ctx context.Context, projectID, kind, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	switch kind {
	case model.SecretBindingTargetProject:
		if ref != "" && ref != projectID {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "a project binding is on its own project, and names no other target")
		}
		return projectID, nil
	case model.SecretBindingTargetHarnessConfig:
		if ref == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "a harness config binding names its harness config")
		}
		for _, lookup := range []func(context.Context, string, string) (*model.HarnessConfig, error){
			s.store.GetHarnessConfigBySlug, s.store.GetHarnessConfigByName, s.store.GetHarnessConfig,
		} {
			if config, err := lookup(ctx, projectID, ref); err == nil {
				return config.ID, nil
			} else if !errors.Is(err, store.ErrNotFound) {
				return "", err
			}
		}
		return "", apperrors.NewStatusError(http.StatusNotFound, fmt.Sprintf("harness config %s not found", ref))
	case model.SecretBindingTargetSandbox:
		if ref == "" {
			return "", apperrors.NewStatusError(http.StatusBadRequest, "a discobox binding names its discobox")
		}
		if sandbox, err := s.store.GetSandboxByName(ctx, projectID, ref); err == nil {
			return sandbox.ID, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
		sandbox, err := s.store.GetSandbox(ctx, projectID, ref)
		if err != nil {
			return "", apperrors.NotFound(err, fmt.Sprintf("discobox %s not found", ref))
		}
		return sandbox.ID, nil
	}
	return "", apperrors.NewStatusError(http.StatusBadRequest, "a binding's target is project, harnessConfig or sandbox")
}

// nameBindings fills in each binding's secret name.
func (s *Service) nameBindings(ctx context.Context, projectID string, bindings []model.SecretBinding) {
	names := map[string]string{}
	for i := range bindings {
		name, ok := names[bindings[i].SecretID]
		if !ok {
			if secret, err := s.store.GetSecret(ctx, projectID, bindings[i].SecretID); err == nil {
				name = secret.Name
			}
			names[bindings[i].SecretID] = name
		}
		bindings[i].SecretName = name
	}
}

// bindingConflict turns a uniqueness constraint on bindings into an answer.
func bindingConflict(err error) error {
	if err == nil || !isUniqueViolation(err) {
		return apperrors.NotFound(err, "the binding's target was not found")
	}
	return apperrors.NewStatusError(http.StatusConflict,
		"that target already has a binding for this variable, or already delegates this secret; change or remove that one")
}

// SandboxBindings are the bindings a discobox is created with, built and
// checked but not yet stored: they are stored in the transaction that stores
// the discobox (StoreSandboxBindingsTx), so a create whose bindings cannot all
// be made creates nothing.
type SandboxBindings struct {
	Bindings []*model.SecretBinding
	// Parents are, when a discobox makes them, the delegate bindings each may
	// be handed on under, by index, longest-lived first; nil when a person
	// makes them.
	Parents [][]model.SecretBinding
	// Grantor is the discobox making them, when one is.
	Grantor string
	// Actor is who the events record.
	Actor string
	// named records, by index, whether the lifetime was named.
	named []bool
	ttls  []int64
}

// PrepareSandboxBindings checks the bindings a discobox is being created with
// (ADR 26-10-27-880 §5) and builds them on it. A person may bind any secret,
// for use or delegation. A discobox may bind on the discoboxes it creates only
// what it was delegated, for use alone: each is handed on under a live
// delegate binding covering it of the same secret, and a lifetime it did not
// name is fitted to that.
func (s *Service) PrepareSandboxBindings(ctx context.Context, projectID string, sandbox *model.Sandbox, requested []apimodel.SandboxBindingInput) (SandboxBindings, error) {
	var out SandboxBindings
	if len(requested) == 0 {
		return out, nil
	}
	principal, _ := auth.PrincipalFromContext(ctx)
	out.Actor = principal.Actor()
	byDiscobox := principal.Type == auth.PrincipalTypeSandbox
	if byDiscobox {
		out.Grantor = principal.SandboxID
	}
	bound := map[string]bool{}
	delegated := map[string]bool{}
	for _, in := range requested {
		delegate := in.Delegate.Or(false)
		binding := &model.SecretBinding{
			ProjectID:  projectID,
			TargetKind: model.SecretBindingTargetSandbox,
			TargetID:   sandbox.ID,
			Use:        in.Use.Or(!delegate),
			Delegate:   delegate,
			EnvName:    strings.TrimSpace(in.EnvVar.Or("")),
			Raw:        string(in.Raw.Or("")),
			CreatedBy:  out.Actor,
		}
		var err error
		if binding.Routes, err = routesFrom(in.Routes); err != nil {
			return SandboxBindings{}, err
		}
		if err := checkBindingShape(binding); err != nil {
			return SandboxBindings{}, err
		}
		var secret *model.Secret
		var parents []model.SecretBinding
		if byDiscobox {
			if binding.Delegate {
				return SandboxBindings{}, apperrors.NewStatusError(http.StatusForbidden,
					"a discobox never delegates: handing on the power to hand on is a person's")
			}
			if parents, secret, err = s.delegationsFor(ctx, projectID, principal.SandboxID, strings.TrimSpace(in.Secret)); err != nil {
				return SandboxBindings{}, err
			}
			binding.ParentID = parents[0].ID
		} else if secret, err = s.lookupSecret(ctx, projectID, in.Secret); err != nil {
			return SandboxBindings{}, err
		}
		binding.SecretID, binding.SecretName = secret.ID, secret.Name
		if err := checkRawBinding(ctx, binding, secret); err != nil {
			return SandboxBindings{}, err
		}
		if binding.Use {
			if bound[binding.EnvName] {
				return SandboxBindings{}, apperrors.NewStatusError(http.StatusBadRequest,
					fmt.Sprintf("%s is given two credentials; one environment variable carries one", binding.EnvName))
			}
			bound[binding.EnvName] = true
		}
		if binding.Delegate {
			if delegated[secret.ID] {
				return SandboxBindings{}, apperrors.NewStatusError(http.StatusBadRequest,
					fmt.Sprintf("%s is delegated twice", secret.Name))
			}
			delegated[secret.ID] = true
		}
		ttl, named := in.TtlSeconds.Get()
		if !named {
			ttl = secret.MaxBindingTTL
		}
		if ttl < 0 {
			return SandboxBindings{}, apperrors.NewStatusError(http.StatusBadRequest, "a binding's lifetime is a number of seconds; 0 is forever")
		}
		if !byDiscobox || named {
			if err := guardBindingTTL(secret, ttl); err != nil {
				return SandboxBindings{}, err
			}
		}
		out.Bindings = append(out.Bindings, binding)
		out.Parents = append(out.Parents, parents)
		out.named = append(out.named, named)
		out.ttls = append(out.ttls, ttl)
	}
	return out, nil
}

// StoreSandboxBindingsTx stores the bindings a create gives the new
// discobox, in the transaction that stores it. Made by a discobox, each is
// held again to a delegate binding still covering it, and its lifetime fitted
// to it then: a delegate binding removed since the bindings were prepared
// refuses the create, which then creates nothing.
func StoreSandboxBindingsTx(ctx context.Context, txStore *store.Store, prepared SandboxBindings) error {
	now := time.Now().UTC()
	for i, binding := range prepared.Bindings {
		ttl := prepared.ttls[i]
		if prepared.Grantor != "" {
			parent, err := heldParent(ctx, txStore, binding.ProjectID, prepared.Grantor, binding.SecretID, prepared.Parents[i], now)
			if err != nil {
				return err
			}
			if ttl, err = fitToParent(parent, ttl, prepared.named[i], now); err != nil {
				return err
			}
			binding.ParentID = parent.ID
		}
		binding.ExpiresAt = expiryAt(now, ttl)
		if err := txStore.CreateSecretBinding(ctx, binding, store.BindingChange{Actor: prepared.Actor}); err != nil {
			return bindingConflict(err)
		}
	}
	return nil
}
