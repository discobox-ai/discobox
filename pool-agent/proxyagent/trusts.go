package proxyagent

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

// The pool half of host trust (ADR 0149). It lives in the proxy unit for the
// reason the credential broker does: the chain a person is shown has to be the
// one this proxy meets, so the probe goes out through the proxy's own
// upstream path, and a pin a person approves has to reach the proxy before
// the agent that asked for it sends its next request.

// trustRefreshInterval bounds how long a revoked trust keeps being honored,
// and how long a trust approved without the agent polling for it waits to
// take effect.
const trustRefreshInterval = 30 * time.Second

// hostTrusts probes hosts for the broker and keeps the proxy's pins in step
// with the control plane's live trusts for this pool.
type hostTrusts struct {
	server       *proxy.Server
	controlPlane *controlPlaneCredentials
	publisher    *policyPublisher
	onError      func(error)

	// mu serializes refreshes, so two that race cannot apply an older read
	// over a newer one.
	mu sync.Mutex
}

func newHostTrusts(server *proxy.Server, controlPlane *controlPlaneCredentials, publisher *policyPublisher, onError func(error)) *hostTrusts {
	return &hostTrusts{server: server, controlPlane: controlPlane, publisher: publisher, onError: onError}
}

// run refreshes on start and then on trustRefreshInterval until ctx is done.
func (h *hostTrusts) run(ctx context.Context) {
	ticker := time.NewTicker(trustRefreshInterval)
	defer ticker.Stop()
	for {
		if _, err := h.refresh(ctx); err != nil && h.onError != nil && ctx.Err() == nil {
			h.onError(fmt.Errorf("refresh host trusts: %w", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// refresh reads the pool's live trusts, applies them to the proxy, and
// returns them. A trust the proxy would refuse is reported and left out
// rather than failing the rest: one malformed row must not take every other
// sandbox's pins down with it.
func (h *hostTrusts) refresh(ctx context.Context) ([]hostTrustDoc, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	docs, err := h.controlPlane.listPoolHostTrusts(ctx)
	if err != nil {
		return nil, err
	}
	trusts := make([]proxy.HostTrust, 0, len(docs))
	for _, doc := range docs {
		trust := doc.proxyTrust()
		if err := trust.Validate(); err != nil {
			if h.onError != nil {
				h.onError(fmt.Errorf("skip host trust %s: %w", doc.ID, err))
			}
			continue
		}
		trusts = append(trusts, trust)
	}
	h.publisher.setTrusts(trusts)
	return docs, nil
}

// probe connects to endpoint as sandboxID's traffic would and describes what
// it was shown. A host the sandbox may not reach is refused before any
// connection is made.
func (h *hostTrusts) probe(ctx context.Context, sandboxID, endpoint string) (proxy.TLSProbe, error) {
	result, err := h.server.ProbeTLS(ctx, sandboxID, endpoint)
	if errors.Is(err, proxy.ErrProbeHostDenied) {
		return proxy.TLSProbe{}, fmt.Errorf("%w: this sandbox may not reach %s at all, so there is nothing to trust", agentcreds.ErrDenied, endpoint)
	}
	return result, err
}

// suppliedCA parses the CA an agent offered and checks the observed chain
// verifies against it for the endpoint's name. An agent is never allowed to
// put a certificate in front of a person that the host did not answer to.
func suppliedCA(value, endpoint string, chain []*x509.Certificate) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%w: suppliedCA is not a PEM certificate", agentcreds.ErrInvalid)
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: parse suppliedCA: %w", agentcreds.ErrInvalid, err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	host, _, _ := net.SplitHostPort(endpoint)
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: host}); err != nil {
		return nil, fmt.Errorf("%w: the chain %s presents does not verify against the CA you supplied: %w", agentcreds.ErrInvalid, endpoint, err)
	}
	return ca, nil
}

// describeCertificate is what a person is shown of a certificate, and what a
// pin is chosen from.
func describeCertificate(cert *x509.Certificate) agentcreds.Certificate {
	ips := make([]string, 0, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		ips = append(ips, ip.String())
	}
	selfSigned := bytes.Equal(cert.RawIssuer, cert.RawSubject) && cert.CheckSignatureFrom(cert) == nil
	return agentcreds.Certificate{
		Subject:    cert.Subject.String(),
		Issuer:     cert.Issuer.String(),
		DNSNames:   cert.DNSNames,
		IPs:        ips,
		NotBefore:  cert.NotBefore.UTC(),
		NotAfter:   cert.NotAfter.UTC(),
		SHA256:     proxy.CertificateSHA256(cert),
		SPKISHA256: proxy.SPKISHA256(cert),
		IsCA:       cert.IsCA,
		SelfSigned: selfSigned,
		PEM:        string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})),
	}
}

func describeChain(chain []*x509.Certificate) []agentcreds.Certificate {
	out := make([]agentcreds.Certificate, 0, len(chain))
	for _, cert := range chain {
		out = append(out, describeCertificate(cert))
	}
	return out
}

// The control plane's host trust documents. A certificate travels in the
// protocol's own shape, which the control plane's ObservedCertificate
// mirrors field for field.

type trustPinDoc struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}

type hostTrustDoc struct {
	ID        string             `json:"id"`
	SandboxID string             `json:"sandboxId"`
	Host      string             `json:"host"`
	Pin       trustPinDoc        `json:"pin"`
	PinPEM    string             `json:"pinPem,omitempty"`
	Uses      []credentialUseDoc `json:"uses,omitempty"`
	ExpiresAt time.Time          `json:"expiresAt"`
}

type listHostTrustsDoc struct {
	HostTrusts []hostTrustDoc `json:"hostTrusts"`
}

type createTrustRequestDoc struct {
	SandboxID       string                   `json:"sandboxId"`
	Host            string                   `json:"host"`
	Justification   string                   `json:"justification,omitempty"`
	Uses            []credentialUseDoc       `json:"uses"`
	GrantTTLSeconds int64                    `json:"grantTTLSeconds,omitempty"`
	ObservedChain   []agentcreds.Certificate `json:"observedChain"`
	SuppliedCA      *agentcreds.Certificate  `json:"suppliedCA,omitempty"`
}

type trustRequestStatusDoc struct {
	RequestID string             `json:"requestId"`
	Status    string             `json:"status"`
	Host      string             `json:"host"`
	Pin       *trustPinDoc       `json:"pin,omitempty"`
	Uses      []credentialUseDoc `json:"uses,omitempty"`
}

func (d hostTrustDoc) proxyTrust() proxy.HostTrust {
	useIDs := make([]string, 0, len(d.Uses))
	for _, use := range d.Uses {
		useIDs = append(useIDs, use.UseID)
	}
	return proxy.HostTrust{
		ClientID:  d.SandboxID,
		Host:      d.Host,
		Pin:       proxy.TrustPin{Kind: d.Pin.Kind, SHA256: d.Pin.SHA256, PEM: d.PinPEM},
		UseIDs:    useIDs,
		ExpiresAt: d.ExpiresAt,
	}
}

func (d trustRequestStatusDoc) protocol() agentcreds.TrustRequestStatus {
	status := agentcreds.TrustRequestStatus{RequestID: d.RequestID, Status: d.Status, Host: d.Host, Uses: protocolUses(d.Uses, nil)}
	if d.Pin != nil {
		status.Pin = &agentcreds.Pin{Kind: d.Pin.Kind, SHA256: d.Pin.SHA256}
	}
	return status
}

func (c *controlPlaneCredentials) createTrustRequest(ctx context.Context, body createTrustRequestDoc) (trustRequestStatusDoc, error) {
	var out trustRequestStatusDoc
	err := c.do(ctx, http.MethodPost, "sandbox-trust-requests", nil, body, &out)
	return out, err
}

func (c *controlPlaneCredentials) trustRequestStatus(ctx context.Context, sandboxID, requestID string) (trustRequestStatusDoc, error) {
	var out trustRequestStatusDoc
	query := url.Values{"sandboxId": []string{sandboxID}}
	err := c.do(ctx, http.MethodGet, "sandbox-trust-requests/"+url.PathEscape(requestID), query, nil, &out)
	return out, err
}

func (c *controlPlaneCredentials) listPoolHostTrusts(ctx context.Context) ([]hostTrustDoc, error) {
	var out listHostTrustsDoc
	if err := c.do(ctx, http.MethodGet, "sandbox-host-trusts", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.HostTrusts, nil
}

// Trusts lists the hosts this sandbox trusts. It reads the pool's trusts
// afresh and applies them on the way, so what the agent is told is what the
// proxy is enforcing.
func (b *credentialBroker) Trusts(ctx context.Context) ([]agentcreds.Trust, error) {
	docs, err := b.trusts.refresh(ctx)
	if err != nil {
		return nil, err
	}
	out := []agentcreds.Trust{}
	for _, doc := range docs {
		if doc.SandboxID != b.sandboxID {
			continue
		}
		expiresAt := doc.ExpiresAt
		out = append(out, agentcreds.Trust{
			Host:      doc.Host,
			Pin:       agentcreds.Pin{Kind: doc.Pin.Kind, SHA256: doc.Pin.SHA256},
			Uses:      protocolUses(doc.Uses, &expiresAt),
			ExpiresAt: &expiresAt,
		})
	}
	return out, nil
}

// RequestTrust probes the host through the proxy and, when the chain it is
// shown is one the proxy would refuse, records the ask with that chain. A
// chain the proxy already accepts settles the ask here, with nobody asked.
func (b *credentialBroker) RequestTrust(ctx context.Context, body agentcreds.TrustRequestBody) (agentcreds.TrustRequestStatus, error) {
	endpoint := agentcreds.TrustHost(body.Host)
	if endpoint == "" {
		return agentcreds.TrustRequestStatus{}, fmt.Errorf("%w: name the host to trust as host or host:port", agentcreds.ErrInvalid)
	}
	probe, err := b.trusts.probe(ctx, b.sandboxID, endpoint)
	if err != nil {
		return agentcreds.TrustRequestStatus{}, err
	}
	chain := describeChain(probe.Chain)
	if probe.Verified {
		reason := "the certificate " + endpoint + " presents already verifies; nothing needs trusting"
		if probe.Via != "" {
			// This pool's egress goes through another proxy, which checks the
			// host itself. What verified here is what that proxy presented —
			// its own certificate when it intercepts TLS, as a discobox's does
			// — so if the host is still refused, it is refused there, and has
			// to be trusted where that proxy runs.
			reason = "the certificate verifies as the upstream proxy " + probe.Via + " presents it; this pool reaches " +
				endpoint + " through that proxy, so if the host is still refused, it has to be trusted where that proxy runs"
		}
		return agentcreds.TrustRequestStatus{
			Status:        agentcreds.StatusUnneeded,
			Host:          endpoint,
			Reason:        reason,
			ObservedChain: chain,
		}, nil
	}
	var supplied *agentcreds.Certificate
	if strings.TrimSpace(body.SuppliedCA) != "" {
		ca, err := suppliedCA(body.SuppliedCA, endpoint, probe.Chain)
		if err != nil {
			return agentcreds.TrustRequestStatus{}, err
		}
		described := describeCertificate(ca)
		supplied = &described
	}
	uses := make([]credentialUseDoc, 0, len(body.Uses))
	for _, use := range body.Uses {
		uses = append(uses, credentialUseDoc{Description: use.Description})
	}
	doc, err := b.controlPlan.createTrustRequest(ctx, createTrustRequestDoc{
		SandboxID:       b.sandboxID,
		Host:            endpoint,
		Justification:   body.Justification,
		Uses:            uses,
		GrantTTLSeconds: body.GrantTTLSeconds,
		ObservedChain:   chain,
		SuppliedCA:      supplied,
	})
	if err != nil {
		return agentcreds.TrustRequestStatus{}, err
	}
	status := doc.protocol()
	status.ObservedChain = chain
	return status, nil
}

// TrustRequestStatus polls a trust request. A granted one is applied to the
// proxy before it is reported, so an agent that sends its next request the
// moment it reads "granted" finds the pin already in force.
func (b *credentialBroker) TrustRequestStatus(ctx context.Context, requestID string) (agentcreds.TrustRequestStatus, error) {
	doc, err := b.controlPlan.trustRequestStatus(ctx, b.sandboxID, requestID)
	if err != nil {
		return agentcreds.TrustRequestStatus{}, err
	}
	if doc.Status == agentcreds.StatusGranted {
		if _, err := b.trusts.refresh(ctx); err != nil {
			return agentcreds.TrustRequestStatus{}, fmt.Errorf("the trust was granted, but could not be applied yet: %w", err)
		}
	}
	return doc.protocol(), nil
}
