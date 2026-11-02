package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/credentialpolicy"
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
		credential.Raw = applied.Binding.Raw
	}
	return credential
}

// CreateSandboxCredentialRequest records an agent's ask as a pending
// SecretRequest and returns immediately. Approval is a human act with human
// latency, so this never waits for one; the caller polls
// GetSandboxCredentialRequest.
//
// An ask has a use part, a delegate part, or both (ADR 26-11-02-022 §1), each
// naming its routes inline or by policy documents, which are resolved here
// and nowhere else (§2): what the request stores, and everything an approver
// is later shown of it, comes from this one resolution.
//
// An identical pending ask is reused rather than duplicated, so an agent that
// retries produces one inbox item instead of a pile. Identical means approving
// either would grant the same thing (store.FindPendingAgentCredentialRequests).
func (s *Service) CreateSandboxCredentialRequest(ctx context.Context, poolID string, input services.CreateSandboxCredentialRequestBody) (*model.SecretRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, strings.TrimSpace(input.SandboxId))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(input.Name)
	envName := strings.TrimSpace(input.EnvVar.Or(""))
	if name == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential name is required")
	}
	if count := len(input.Policies) + len(input.Delegate.Or(services.SandboxCredentialRequestDelegate{}).Policies); count > agentcreds.MaxRequestPolicies {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a request names at most %d policies across its parts, not %d", agentcreds.MaxRequestPolicies, count))
	}
	hosts := hostscope.List(input.Hosts...)
	budget := agentcreds.MaxRequestRouteBytes
	use, err := requestedPart("use", hosts, input.Routes, input.Policies, &budget)
	if err != nil {
		return nil, err
	}
	use.Asked = use.Asked || envName != ""
	var delegate model.SecretRequestPart
	if part, ok := input.Delegate.Get(); ok {
		if delegate, err = requestedPart("delegate", nil, part.Routes, part.Policies, &budget); err != nil {
			return nil, err
		}
		if !delegate.Asked {
			return nil, apperrors.NewStatusError(http.StatusBadRequest,
				"the delegate part names where the credential may be sent: routes or policies")
		}
	}
	switch {
	case !use.Asked && !delegate.Asked:
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"credential request requires a destination: a host, a route or a policy to use it, or a delegate part")
	case delegate.Asked && envName != "" && len(use.Routes) == 0:
		// A delegation delivers nothing to the discobox that asks, so a
		// variable with nothing to use is a mistake about what it asks for.
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"a request with only a delegate part names no environment variable: a delegation delivers nothing to the discobox that asks")
	case use.Asked && len(use.Routes) == 0:
		// A destination is mandatory: the routes are what an approval binds
		// unless the approver names others, and an agent says where it means
		// to send what it asks for (ADR 0031 §5).
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			"credential request requires a destination to use it: a host, a route or a policy")
	case use.Asked && !services.EnvVarNamePattern.MatchString(envName):
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "credential request requires a valid environment variable name to use it")
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
	req := &model.SecretRequest{
		ProjectID:     sandbox.ProjectID,
		RequestedBy:   agentRequesterID(sandbox.ID),
		SandboxID:     sandbox.ID,
		Name:          name,
		EnvName:       envName,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		TTL:           grantTTL,
		Use:           use,
		Delegate:      delegate,
		Status:        model.SecretRequestStatusPending,
	}
	pending, err := s.store.FindPendingAgentCredentialRequests(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(pending) > 0 {
		return &pending[0], nil
	}
	if err := s.store.CreateSecretRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// requestedPart reads one part of an ask: its routes, inline (hosts and typed
// routes) or by policy documents, never both. Each document is resolved
// against its arguments (credentialpolicy.ResolveDocument) and recorded with
// what an approver is shown of it; the part's routes are the union of them.
// The part is Asked when it names anything at all. Input from a box: every
// limit here is a boundary (ADR 26-11-02-022 §2), and budget, shared by the
// request's parts, is what is left of agentcreds.MaxRequestRouteBytes: it is
// spent after each document resolves, so a request is refused as soon as
// what it resolves to outgrows it, not after all of it was expanded.
func requestedPart(which string, hosts, typed []string, policies []services.SandboxCredentialPolicy, budget *int) (model.SecretRequestPart, error) {
	var part model.SecretRequestPart
	if len(policies) > 0 && (len(hosts) > 0 || len(typed) > 0) {
		return part, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the %s part names its routes inline or by policies, not both", which))
	}
	switch {
	case len(policies) > 0:
		for i, policy := range policies {
			resolved, err := requestedPolicy(policy)
			if err != nil {
				return part, apperrors.NewStatusError(http.StatusBadRequest,
					fmt.Sprintf("%s part, policy %d (%q): %v", which, i+1, policy.File, err))
			}
			if err := spendRouteBytes(budget, resolved.Routes); err != nil {
				return part, err
			}
			part.Policies = append(part.Policies, resolved)
			part.Routes = append(part.Routes, resolved.Routes...)
		}
		routes, err := model.NormalizeRoutes(part.Routes)
		if err != nil {
			return part, apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("%s part: %v", which, err))
		}
		part.Routes = routes
	case len(hosts) > 0 || len(typed) > 0:
		routes, err := requestedRoutes(hosts, typed)
		if err != nil {
			return part, err
		}
		part.Routes = routes
	default:
		return part, nil
	}
	if len(part.Routes) > agentcreds.MaxPartRoutes {
		return part, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the %s part resolves to %d routes; a part may have at most %d", which, len(part.Routes), agentcreds.MaxPartRoutes))
	}
	if err := spendRouteBytes(budget, part.Routes); err != nil {
		return part, err
	}
	part.Asked = true
	return part, nil
}

// spendRouteBytes takes the JSON size of routes from what is left of a
// request's agentcreds.MaxRequestRouteBytes, refusing the request, naming the
// limit, once it is spent.
func spendRouteBytes(budget *int, routes []model.SecretRoute) error {
	encoded, err := json.Marshal(routes)
	if err != nil {
		return err
	}
	*budget -= len(encoded)
	if *budget < 0 {
		return apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("the request resolves to more than %d bytes of routes, the most a request may (its policies' arguments expand into its routes)", agentcreds.MaxRequestRouteBytes))
	}
	return nil
}

// requestedPolicy resolves one policy document a box sent, and records what
// an approver is shown of it: the file's base name, its description, its
// arguments, a digest of the document and the arguments no route path names
// — beside the routes it resolved to, which are what it grants (ADR
// 26-11-02-022 §3).
func requestedPolicy(policy services.SandboxCredentialPolicy) (model.SecretRequestPolicy, error) {
	file := strings.TrimSpace(policy.File)
	if file == "" {
		return model.SecretRequestPolicy{}, errors.New("a policy names its file")
	}
	if len(policy.Document) > agentcreds.MaxPolicyBytes {
		return model.SecretRequestPolicy{}, fmt.Errorf("the document is %d bytes; a policy may be at most %d", len(policy.Document), agentcreds.MaxPolicyBytes)
	}
	args := map[string]string(policy.Args.Or(nil))
	resolved, err := credentialpolicy.ResolveDocument(policy.Document, args)
	if err != nil {
		return model.SecretRequestPolicy{}, err
	}
	routes, err := model.NormalizeRoutes(resolved.Routes)
	if err != nil {
		return model.SecretRequestPolicy{}, err
	}
	name := strings.TrimSuffix(path.Base(strings.ReplaceAll(file, "\\", "/")), ".json")
	if len(args) == 0 {
		args = nil
	}
	return model.SecretRequestPolicy{
		File:        file,
		Name:        name,
		Description: resolved.Description,
		Args:        args,
		Digest:      resolved.Digest,
		UnusedArgs:  resolved.UnusedArgs,
		Routes:      routes,
	}, nil
}

// GetSandboxCredentialRequest reads one of a sandbox's own credential requests
// and, for each part approved, the binding it was answered with while it
// stands.
func (s *Service) GetSandboxCredentialRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.SecretRequest, services.RequestBindings, error) {
	var bindings services.RequestBindings
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, bindings, err
	}
	req, err := s.store.GetSecretRequest(ctx, sandbox.ProjectID, requestID)
	if err != nil {
		return nil, bindings, apperrors.NotFound(err, "secret request not found")
	}
	// A sandbox may poll only its own asks. Requests are project-scoped rows and
	// a pool hosts many sandboxes, so without this a compromised sandbox could
	// read every request in the project by guessing IDs.
	if req.SandboxID != sandbox.ID {
		return nil, bindings, apperrors.NewStatusError(http.StatusNotFound, "secret request not found")
	}
	for _, part := range []struct {
		part *model.SecretRequestPart
		into **model.SecretBinding
	}{{&req.Use, &bindings.Use}, {&req.Delegate, &bindings.Delegate}} {
		if part.part.Outcome != model.SecretRequestPartApproved || part.part.BindingID == "" {
			continue
		}
		binding, err := s.store.SecretBindingByExactID(ctx, sandbox.ProjectID, part.part.BindingID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// The binding was removed after approval. The part stays
				// approved (it is history), but there is nothing to use.
				continue
			}
			return nil, bindings, err
		}
		if binding.Live(time.Now().UTC()) {
			*part.into = binding
		}
	}
	return req, bindings, nil
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
