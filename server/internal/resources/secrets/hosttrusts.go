package secrets

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Host trust (ADR 0149). An agent asks, through its pool, for a host whose
// certificate the pool's egress refuses to be trusted for its own sandbox; a
// person pins one certificate from the chain the pool observed; the pool's
// proxy enforces the pin on that sandbox's traffic alone. It lives beside the
// credential broker because it is the same act — an agent's ask, a person's
// approval, uses the judge reads — about a different thing.

// CreateSandboxTrustRequest records a pool's relay of an agent's ask. The
// chain is the pool's observation, never the sandbox's word: the pool
// connected to the host itself. A pending ask for the same host is reused.
func (s *Service) CreateSandboxTrustRequest(ctx context.Context, poolID string, input services.CreateSandboxTrustRequestBody) (*model.HostTrustRequest, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, input.SandboxId)
	if err != nil {
		return nil, err
	}
	host := agentcreds.TrustHost(input.Host)
	if host == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a trust request names one host, as host or host:port")
	}
	uses, err := requestedUses(input.Uses)
	if err != nil {
		return nil, err
	}
	grantTTL := input.GrantTTLSeconds.Or(0)
	if grantTTL < 0 || grantTTL > model.MaxHostTrustTTLSeconds {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a requested trust lifetime runs from 1 second to %d (thirty days); leave it out to ask for nothing in particular", model.MaxHostTrustTTLSeconds))
	}
	if len(input.ObservedChain) == 0 {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "a trust request carries the chain the pool observed")
	}

	existing, err := s.store.FindPendingHostTrustRequest(ctx, sandbox.ProjectID, sandbox.ID, host)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if existing != nil {
		return existing, nil
	}
	req := &model.HostTrustRequest{
		ProjectID:     sandbox.ProjectID,
		SandboxID:     sandbox.ID,
		RequestedBy:   agentRequesterID(sandbox.ID),
		Host:          host,
		Justification: strings.TrimSpace(input.Justification.Or("")),
		Uses:          uses,
		GrantTTL:      grantTTL,
		ObservedChain: observedChain(input.ObservedChain),
		Status:        model.HostTrustRequestStatusPending,
	}
	if supplied, ok := input.SuppliedCA.Get(); ok {
		ca := observedCertificate(supplied)
		req.SuppliedCA = &ca
	}
	if err := s.store.CreateHostTrustRequest(ctx, req); err != nil {
		return nil, err
	}
	return req, nil
}

// GetSandboxTrustRequest returns one of the sandbox's own trust requests and,
// once approved, the trust it minted while that trust is live. An approval
// whose trust has since lapsed or been revoked comes back with no trust, which
// the pool reports as denied, as a credential's revoked grant is.
func (s *Service) GetSandboxTrustRequest(ctx context.Context, poolID, sandboxID, requestID string) (*model.HostTrustRequest, *model.HostTrust, error) {
	sandbox, err := s.sandboxOwnedByPool(ctx, poolID, sandboxID)
	if err != nil {
		return nil, nil, err
	}
	req, err := s.store.GetHostTrustRequest(ctx, sandbox.ProjectID, requestID)
	if err != nil || req.SandboxID != sandbox.ID {
		// Another sandbox's request is not this one's to see, and saying it
		// exists would tell it so.
		return nil, nil, apperrors.NotFound(store.ErrNotFound, "trust request not found")
	}
	if req.Status != model.HostTrustRequestStatusApproved || req.TrustID == "" {
		return req, nil, nil
	}
	trust, err := s.store.GetHostTrust(ctx, sandbox.ProjectID, req.TrustID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return req, nil, nil
		}
		return nil, nil, err
	}
	if !trust.Live(time.Now()) {
		return req, nil, nil
	}
	return req, trust, nil
}

// ListPoolHostTrusts returns the live trusts of every sandbox on the pool:
// what the pool's proxy enforces.
func (s *Service) ListPoolHostTrusts(ctx context.Context, poolID string) ([]model.HostTrust, error) {
	if strings.TrimSpace(poolID) == "" {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, "pool ID is required")
	}
	return s.store.ListLivePoolHostTrusts(ctx, poolID, time.Now())
}

// ListTrustRequests is the project's trust-request inbox.
func (s *Service) ListTrustRequests(ctx context.Context, projectID, status string) ([]model.HostTrustRequest, error) {
	if _, err := s.store.GetProject(ctx, projectID); err != nil {
		return nil, apperrors.NotFound(err, "project not found")
	}
	return s.store.ListHostTrustRequests(ctx, projectID, status)
}

func (s *Service) GetTrustRequest(ctx context.Context, projectID, requestID string) (*model.HostTrustRequest, error) {
	req, err := s.store.GetHostTrustRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "trust request not found")
	}
	return req, nil
}

// ApproveTrustRequest pins one certificate from what the pool observed and
// mints the sandbox's trust. The pin must be one the request offers — a CA in
// the observed chain, the CA the agent supplied, or the leaf's key — so an
// approver is only ever agreeing to what the pool actually met. It is a
// person's act: a discobox answering the inbox is refused, because a pin
// decides who the sandbox's credentials for that host are handed to.
func (s *Service) ApproveTrustRequest(ctx context.Context, projectID, requestID string, input services.ApproveTrustRequestBody) (*model.HostTrustRequest, error) {
	principal, _ := auth.PrincipalFromContext(ctx)
	if principal.Type == auth.PrincipalTypeSandbox {
		return nil, apperrors.NewStatusError(http.StatusForbidden, "a host is trusted only by a person")
	}
	req, err := s.store.GetHostTrustRequest(ctx, projectID, requestID)
	if err != nil {
		return nil, apperrors.NotFound(err, "trust request not found")
	}
	if req.Status != model.HostTrustRequestStatusPending {
		return nil, apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("trust request is already %s", req.Status))
	}
	pin := DefaultTrustPin(req)
	if chosen, ok := input.Pin.Get(); ok {
		pin = model.TrustPin{Kind: string(chosen.Kind), SHA256: strings.ToLower(strings.TrimSpace(chosen.SHA256))}
	}
	pinPEM, err := pinnedCertificate(req, pin)
	if err != nil {
		return nil, err
	}
	uses := req.Uses
	if edited, ok := input.Uses.Get(); ok && edited != nil {
		if uses = convertAPIUses(edited); len(uses) == 0 {
			return nil, apperrors.NewStatusError(http.StatusBadRequest, "an approved trust needs at least one use")
		}
	}
	uses, err = mintUseIDs(uses)
	if err != nil {
		return nil, err
	}
	ttl := req.GrantTTL
	if chosen, ok := input.GrantTTLSeconds.Get(); ok {
		ttl = chosen
	}
	if ttl == 0 {
		ttl = model.DefaultHostTrustTTLSeconds
	}
	if ttl < 0 || ttl > model.MaxHostTrustTTLSeconds {
		return nil, apperrors.NewStatusError(http.StatusBadRequest,
			fmt.Sprintf("a trust lasts from 1 second to %d (thirty days); a trust always lapses", model.MaxHostTrustTTLSeconds))
	}
	trust := &model.HostTrust{
		ProjectID: req.ProjectID,
		SandboxID: req.SandboxID,
		Host:      req.Host,
		Pin:       pin,
		PinPEM:    pinPEM,
		Uses:      uses,
		ExpiresAt: time.Now().UTC().Add(time.Duration(ttl) * time.Second),
		GrantedBy: grantedByOf(principal),
		RequestID: req.ID,
	}
	if err := s.store.ApproveHostTrustRequest(ctx, req, trust); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return nil, apperrors.NewStatusError(http.StatusConflict, "trust request was answered by someone else")
		}
		return nil, err
	}
	return req, nil
}

func (s *Service) DenyTrustRequest(ctx context.Context, projectID, requestID string) error {
	req, err := s.store.GetHostTrustRequest(ctx, projectID, requestID)
	if err != nil {
		return apperrors.NotFound(err, "trust request not found")
	}
	if req.Status != model.HostTrustRequestStatusPending {
		return apperrors.NewStatusError(http.StatusConflict, fmt.Sprintf("trust request is already %s", req.Status))
	}
	req.Status = model.HostTrustRequestStatusDenied
	if err := s.store.UpdateHostTrustRequestIfPending(ctx, req); err != nil {
		if errors.Is(err, store.ErrGenerationConflict) {
			return apperrors.NewStatusError(http.StatusConflict, "trust request was answered by someone else")
		}
		return err
	}
	return nil
}

// ListSandboxHostTrusts returns the trusts a sandbox holds now.
func (s *Service) ListSandboxHostTrusts(ctx context.Context, projectID, sandboxID string) ([]model.HostTrust, error) {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return nil, apperrors.NotFound(err, "sandbox not found")
	}
	return s.store.ListLiveSandboxHostTrusts(ctx, projectID, sandbox.ID, time.Now())
}

// DeleteSandboxHostTrust revokes a trust. The pool's proxy stops honoring it
// at its next read of the pool's trusts.
func (s *Service) DeleteSandboxHostTrust(ctx context.Context, projectID, sandboxID, trustID string) error {
	sandbox, err := s.store.GetSandbox(ctx, projectID, sandboxID)
	if err != nil {
		return apperrors.NotFound(err, "sandbox not found")
	}
	if err := s.store.DeleteHostTrust(ctx, projectID, sandbox.ID, trustID); err != nil {
		return apperrors.NotFound(err, "host trust not found")
	}
	return nil
}

// DefaultTrustPin is the pin an approval takes when the approver names none,
// and what a window offers first: the CA the agent supplied, then a
// self-signed CA the chain carries, and only then the leaf's key, which breaks
// the day the server's key rotates.
func DefaultTrustPin(req *model.HostTrustRequest) model.TrustPin {
	if req.SuppliedCA != nil {
		return model.TrustPin{Kind: model.TrustPinKindCA, SHA256: req.SuppliedCA.SHA256}
	}
	for i := len(req.ObservedChain) - 1; i >= 0; i-- {
		if cert := req.ObservedChain[i]; cert.IsCA && cert.SelfSigned {
			return model.TrustPin{Kind: model.TrustPinKindCA, SHA256: cert.SHA256}
		}
	}
	if len(req.ObservedChain) == 0 {
		return model.TrustPin{}
	}
	return model.TrustPin{Kind: model.TrustPinKindLeafSPKI, SHA256: req.ObservedChain[0].SPKISHA256}
}

// pinnedCertificate checks that pin names something the request offers and
// returns the PEM the proxy verifies against (none for a leaf key).
func pinnedCertificate(req *model.HostTrustRequest, pin model.TrustPin) (string, error) {
	switch pin.Kind {
	case model.TrustPinKindCA:
		if req.SuppliedCA != nil && req.SuppliedCA.SHA256 == pin.SHA256 {
			return req.SuppliedCA.PEM, nil
		}
		for _, cert := range req.ObservedChain {
			if cert.IsCA && cert.SHA256 == pin.SHA256 {
				return cert.PEM, nil
			}
		}
		return "", apperrors.NewStatusError(http.StatusBadRequest, "a ca pin names a CA in the observed chain or the one the agent supplied")
	case model.TrustPinKindLeafSPKI:
		if len(req.ObservedChain) > 0 && req.ObservedChain[0].SPKISHA256 == pin.SHA256 {
			return "", nil
		}
		return "", apperrors.NewStatusError(http.StatusBadRequest, "a leaf-spki pin names the key of the certificate the host presented")
	default:
		return "", apperrors.NewStatusError(http.StatusBadRequest, "a pin is ca or leaf-spki")
	}
}

func observedChain(input []apimodel.ObservedCertificate) []model.ObservedCertificate {
	out := make([]model.ObservedCertificate, 0, len(input))
	for _, cert := range input {
		out = append(out, observedCertificate(cert))
	}
	return out
}

func observedCertificate(cert apimodel.ObservedCertificate) model.ObservedCertificate {
	return model.ObservedCertificate{
		Subject:    cert.Subject,
		Issuer:     cert.Issuer,
		DNSNames:   cert.DnsNames.Or(nil),
		IPs:        cert.Ips.Or(nil),
		NotBefore:  cert.NotBefore,
		NotAfter:   cert.NotAfter,
		SHA256:     strings.ToLower(cert.SHA256),
		SPKISHA256: strings.ToLower(cert.SpkiSha256),
		IsCA:       cert.IsCA.Or(false),
		SelfSigned: cert.SelfSigned.Or(false),
		PEM:        cert.Pem,
	}
}
