package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/hostscope"
	"github.com/discobox-ai/discobox/pkg/secretroute"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// The agent credentials broker: the control-plane half of the agent
// credentials protocol (ADR 0031, as ADR 26-10-27-880 leaves it). A pool agent
// calls it on behalf of one of its own sandboxes, and every entry point
// re-derives the sandbox from the calling pool rather than trusting the caller
// about which sandbox it is speaking for.
//
// Nothing here hands out a value. The broker records asks and reports what a
// sandbox holds; an approved use is delivered as every credential is, a
// sentinel in the variable it named, and cleartext leaves only through
// ResolveSandboxSecret.

// ListSandboxCredentials returns what a sandbox holds: its effective use
// bindings, each in the variable it delivers, and its live delegate bindings,
// which deliver nothing to it (ADR 26-10-27-880 §2).
func (s *Service) ListSandboxCredentials(ctx context.Context, poolID, sandboxID string) ([]services.SandboxCredential, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, err
	}
	effective, err := s.store.ResolveSandboxBindings(ctx, sandbox, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	secrets := map[string]*model.Secret{}
	secretOf := func(secretID string) *model.Secret {
		if secret, ok := secrets[secretID]; ok {
			return secret
		}
		secret, err := s.store.GetSecret(ctx, sandbox.ProjectID, secretID)
		if err != nil {
			secret = nil
		}
		secrets[secretID] = secret
		return secret
	}
	var out []services.SandboxCredential
	for _, applied := range effective.Use {
		if secret := secretOf(applied.Binding.SecretID); secret != nil {
			out = append(out, sandboxCredential(secret, applied, model.SecretPurposeUse))
		}
	}
	for _, applied := range effective.Delegate {
		if secret := secretOf(applied.Binding.SecretID); secret != nil {
			out = append(out, sandboxCredential(secret, applied, model.SecretPurposeDelegate))
		}
	}
	return out, nil
}

// sandboxCredential is a binding as the protocol lists it: the hosts are
// those the open routes name of the narrowest list it is held to — the binding's when it has
// any, else its parent's, else the secret's.
func sandboxCredential(secret *model.Secret, applied store.EffectiveBinding, purpose string) services.SandboxCredential {
	routes := secret.Routes
	switch {
	case len(applied.Binding.Routes) > 0:
		routes = applied.Binding.Routes
	case applied.Parent != nil && len(applied.Parent.Routes) > 0:
		routes = applied.Parent.Routes
	}
	credential := services.SandboxCredential{
		Name:      secret.Name,
		SecretID:  secret.ID,
		BindingID: applied.Binding.ID,
		Hosts:     model.OpenRouteHosts(routes),
		Purpose:   purpose,
		ExpiresAt: bindingExpiry(&applied.Binding, applied.Parent),
	}
	if purpose == model.SecretPurposeUse {
		credential.EnvName = applied.Binding.EnvName
	}
	return credential
}

// CreateSandboxCredentialRequest records an agent's ask as a pending
// SecretRequest and returns immediately. Approval is a human act with human
// latency, so this never waits for one; the caller polls
// GetSandboxCredentialRequest.
//
// An identical pending ask is reused rather than duplicated, so an agent that
// retries produces one inbox item instead of a pile. Identical means approving
// either would grant the same thing: the same variable, routes (each host
// asked for being one), purpose and lifetime. The justification is left out; it argues for a grant and does not
// change which one approval mints, so a retry reworded is still a retry.
func (s *Service) CreateSandboxCredentialRequest(ctx context.Context, poolID string, input services.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, strings.TrimSpace(input.SandboxId))
	if err != nil {
		return nil, err
	}
	// What the credential is asked for: to use it, or to delegate it.
	purpose, err := requestPurpose(string(input.Purpose.Or("")))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	envName := strings.TrimSpace(input.EnvVar.Or(""))
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential name is required")
	}
	// A use is delivered in the variable it names; a delegation delivers
	// nothing to the discobox that asks, so naming one is a mistake about
	// what it asks for.
	switch {
	case purpose == model.SecretPurposeDelegate && envName != "":
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a request to delegate names no environment variable: a delegation delivers nothing to the discobox that asks")
	case purpose == model.SecretPurposeUse && !services.EnvVarNamePattern.MatchString(envName):
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a valid environment variable name")
	}
	// A destination is mandatory: the request's routes are what an approval
	// binds unless the approver names others, and an agent says where it
	// means to send what it asks for (ADR 0031 §5).
	hosts := hostscope.List(input.Hosts...)
	if len(hosts) == 0 && len(input.Routes) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a destination: a host or a route")
	}
	routes, err := requestedRoutes(hosts, input.Routes)
	if err != nil {
		return nil, err
	}
	// The lifetime asked for is recorded, not checked against any secret: which
	// secret answers the request is the approval's choice, and so is whether
	// its limit is raised to meet the ask.
	//
	// It is bounded on both sides all the same, because an ask becomes the
	// answer a human is shown already chosen. Unbounded, "forever" arrives
	// under another name — a ten-year ask, or one large enough to overflow the
	// duration a window converts it to and land back near zero. The ceiling is
	// the protocol's own (agentcreds.MaxGrantTTLSeconds), so the sandbox-side
	// CLI refuses the same asks this does rather than each end guessing.
	grantTTL := input.GrantTTLSeconds.Or(0)
	if grantTTL < 0 || grantTTL > agentcreds.MaxGrantTTLSeconds {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a requested grant lifetime runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", int64(agentcreds.MaxGrantTTLSeconds)))
	}

	pending, err := s.store.FindPendingAgentCredentialRequests(ctx, sandbox.ProjectID, sandbox.ID, envName, routes, purpose)
	if err != nil {
		return nil, err
	}
	for i := range pending {
		if pending[i].TTL == grantTTL && pending[i].Name == name {
			return &pending[i], nil
		}
	}

	req := &model.SecretRequest{
		ProjectID:     sandbox.ProjectID,
		RequestedBy:   agentRequesterID(sandbox.ID),
		SandboxID:     sandbox.ID,
		Routes:        routes,
		Name:          name,
		EnvName:       envName,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		TTL:           grantTTL,
		Purpose:       purpose,
		Status:        model.SecretRequestStatusPending,
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// GetSandboxCredentialRequest reads one of a sandbox's own credential requests
// and, once approved, the binding it was answered with while it stands.
func (s *Service) GetSandboxCredentialRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.SecretRequest, *model.SecretBinding, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	req, err := s.store.GetSecretRequest(ctx, sandbox.ProjectID, requestID)
	if err != nil {
		return nil, nil, apperrors.NotFound(err, "secret request not found")
	}
	// A sandbox may poll only its own asks. Requests are project-scoped rows and
	// a pool hosts many sandboxes, so without this a compromised sandbox could
	// read every request in the project by guessing IDs.
	if req.SandboxID != sandbox.ID {
		return nil, nil, apperrors.NewStatusError(http.StatusNotFound, "secret request not found")
	}
	if req.Status != model.SecretRequestStatusApproved || req.BindingID == "" {
		return req, nil, nil
	}
	binding, err := s.store.SecretBindingByExactID(ctx, sandbox.ProjectID, req.BindingID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The binding was removed after approval. The request stays
			// approved (it is history), but there is nothing to use.
			return req, nil, nil
		}
		return nil, nil, err
	}
	if !binding.Live(time.Now().UTC()) {
		return req, nil, nil
	}
	return req, binding, nil
}

// requestedRoutes are the routes an ask names, read as every route is written:
// each host one route to that exact host, and each route in the typed form
// (secretroute.Parse). A host that is no host name, or a route that does not
// parse, refuses the ask, naming it, rather than being stored.
func requestedRoutes(hosts, typed []string) ([]model.SecretRoute, error) {
	routes := make([]model.SecretRoute, 0, len(hosts)+len(typed))
	for _, host := range hosts {
		routes = append(routes, model.SecretRoute{Hosts: []string{host}})
	}
	for _, text := range typed {
		route, err := secretroute.Parse(text)
		if err != nil {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
		}
		routes = append(routes, route)
	}
	routes, err := model.NormalizeRoutes(routes)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	return routes, nil
}

// requestPurpose reads what a credential is asked for. Nothing named is use.
func requestPurpose(purpose string) (string, error) {
	switch purpose = strings.TrimSpace(purpose); purpose {
	case "", model.SecretPurposeUse:
		return model.SecretPurposeUse, nil
	case model.SecretPurposeDelegate:
		return purpose, nil
	}
	return "", apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("a credential is asked for use or delegate, not %q", purpose))
}

// agentRequesterID is the principal recorded on a protocol-originated request.
func agentRequesterID(sandboxID string) string { return "agent:" + sandboxID }

// sandboxOwnedByPool resolves a sandbox and verifies the calling pool hosts it,
// the same ownership check ResolveSandboxSecret makes. A pool may only ever
// speak for its own sandboxes.
func (s *Service) sandboxOwnedByPool(ctx context.Context, poolID, sandboxID string) (*model.Sandbox, error) {
	sandboxID = strings.TrimSpace(sandboxID)
	if sandboxID == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "sandbox ID is required")
	}
	sandbox, err := s.store.GetSandboxByID(ctx, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	if strings.TrimSpace(sandbox.PoolID) != strings.TrimSpace(poolID) {
		return nil, apperrors.NewStatusError(http.StatusNotFound, "sandbox not found")
	}
	return sandbox, nil
}
