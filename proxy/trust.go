package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/proxy/internal/audit"
	"github.com/elazarl/goproxy"
	"go.opentelemetry.io/otel/attribute"
)

// Host trust (ADR 0149): a host whose certificate the system roots refuse is
// reachable by one client when that client holds a pin for it. The pin is
// checked on the proxy's own upstream connection; the sandbox side is
// unchanged, since the sandbox only ever talks to the proxy and already
// trusts the MITM CA.

// Pin kinds, as the agent credentials protocol names them.
const (
	// TrustPinCA verifies the chain to a pinned CA and checks the name as
	// usual.
	TrustPinCA = "ca"
	// TrustPinLeafSPKI matches the server certificate's public key exactly,
	// for a server whose chain carries no CA.
	TrustPinLeafSPKI = "leaf-spki"
)

// UntrustedHostHeader names the host:port whose certificate the proxy
// refused, on the 502 it answers with. It is how an agent learns which host
// to ask to have trusted, without reading a sentence.
const UntrustedHostHeader = "X-Discobox-Untrusted-Host"

// UpstreamProxyHeader names the upstream proxy a failed request went out
// through, on the 502 the proxy answers with when the upstream never
// answered. Behind one — a pool nested in a discobox — the failure may be that
// proxy's, and the remedy with whoever runs it.
const UpstreamProxyHeader = "X-Discobox-Upstream-Proxy"

// HostTrust is one client's pin for one endpoint.
type HostTrust struct {
	ClientID string
	// Host is host:port, exactly the endpoint the trust covers.
	Host string
	Pin  TrustPin
	// UseIDs are the approved uses the trust was granted for. Every request
	// the client sends to Host is judged against them.
	UseIDs []string
	// ExpiresAt ends the trust; zero means it does not lapse on its own.
	ExpiresAt time.Time
}

// TrustPin is the certificate a HostTrust verifies against. PEM is required
// for a TrustPinCA, and must hash to SHA256; a TrustPinLeafSPKI names the
// hex SHA-256 of the leaf's SubjectPublicKeyInfo and needs no PEM.
type TrustPin struct {
	Kind   string
	SHA256 string
	PEM    string
}

// Validate reports whether the trust is well-formed: a normalized endpoint,
// a client, and a pin the proxy can verify against.
func (t HostTrust) Validate() error {
	if strings.TrimSpace(t.ClientID) == "" {
		return errors.New("host trust requires a client ID")
	}
	if trustEndpoint(t.Host) != t.Host {
		return fmt.Errorf("host trust %q is not a normalized host:port", t.Host)
	}
	if _, err := hex.DecodeString(t.Pin.SHA256); err != nil || len(t.Pin.SHA256) != sha256.Size*2 {
		return fmt.Errorf("host trust %s: pin is not a hex SHA-256", t.Host)
	}
	switch t.Pin.Kind {
	case TrustPinCA:
		ca, err := parsePEMCertificate(t.Pin.PEM)
		if err != nil {
			return fmt.Errorf("host trust %s: %w", t.Host, err)
		}
		if CertificateSHA256(ca) != t.Pin.SHA256 {
			return fmt.Errorf("host trust %s: pinned CA does not hash to its pin", t.Host)
		}
	case TrustPinLeafSPKI:
	default:
		return fmt.Errorf("host trust %s: unknown pin kind %q", t.Host, t.Pin.Kind)
	}
	return nil
}

// CertificateSHA256 is the hex SHA-256 of a certificate's DER: what a
// TrustPinCA names.
func CertificateSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// SPKISHA256 is the hex SHA-256 of a certificate's SubjectPublicKeyInfo:
// what a TrustPinLeafSPKI names.
func SPKISHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

func parsePEMCertificate(value string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("pinned CA is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// trustEndpoint is the key a trust is held and looked up under: lowercase
// host:port, with 443 filled in. It returns "" for a value that is not one.
func trustEndpoint(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		host, port = strings.Trim(value, "[]"), "443"
	}
	if host == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}

// requestEndpoint is the endpoint an outbound HTTPS request goes to, or ""
// for one that is not HTTPS: a pin is about a TLS connection, and plain HTTP
// makes none.
func requestEndpoint(req *http.Request) string {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" {
		return ""
	}
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	return trustEndpoint(host)
}

// pinnedTrust is a trust together with the transport its connections are
// made on. The transport is the trust's own: http.Transport pools connections
// by host, so a connection verified under one client's pin must never be
// handed to another client's request for the same host, and a transport per
// (client, trust) is what makes that structural.
type pinnedTrust struct {
	HostTrust
	transport *http.Transport
}

func (p *pinnedTrust) live(now time.Time) bool {
	return p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt)
}

// trustTable is the set of pins in force, by client and endpoint. A new
// table is built on every ApplyConfig; a trust that is unchanged keeps its
// transport, and with it its open connections.
type trustTable struct {
	byClient map[string]map[string]*pinnedTrust
}

// trustKey says whether two trusts can share a transport: the same client,
// endpoint, and pin. Uses and expiry do not change a connection.
func trustKey(t HostTrust) string {
	return t.ClientID + "\x00" + t.Host + "\x00" + t.Pin.Kind + "\x00" + t.Pin.SHA256
}

// buildTrustTable builds the table for trusts, reusing previous's transport
// for every trust that is still present, and closing the idle connections of
// every one that is gone.
func buildTrustTable(trusts []HostTrust, base *http.Transport, previous *trustTable) (*trustTable, error) {
	reuse := map[string]*http.Transport{}
	if previous != nil {
		for _, byHost := range previous.byClient {
			for _, pinned := range byHost {
				reuse[trustKey(pinned.HostTrust)] = pinned.transport
			}
		}
	}
	table := &trustTable{byClient: map[string]map[string]*pinnedTrust{}}
	for _, trust := range trusts {
		key := trustKey(trust)
		transport := reuse[key]
		if transport == nil {
			config, err := pinnedTLSConfig(trust.Pin)
			if err != nil {
				return nil, fmt.Errorf("host trust %s: %w", trust.Host, err)
			}
			transport = base.Clone()
			transport.TLSClientConfig = config
		}
		delete(reuse, key)
		byHost := table.byClient[trust.ClientID]
		if byHost == nil {
			byHost = map[string]*pinnedTrust{}
			table.byClient[trust.ClientID] = byHost
		}
		byHost[trust.Host] = &pinnedTrust{HostTrust: trust, transport: transport}
	}
	for _, gone := range reuse {
		gone.CloseIdleConnections()
	}
	return table, nil
}

func (t *trustTable) lookup(clientID, endpoint string, now time.Time) *pinnedTrust {
	if t == nil || clientID == "" || endpoint == "" {
		return nil
	}
	pinned := t.byClient[clientID][endpoint]
	if pinned == nil || !pinned.live(now) {
		return nil
	}
	return pinned
}

// pinnedTLSConfig is the upstream TLS configuration that verifies a host
// against pin and nothing else.
func pinnedTLSConfig(pin TrustPin) (*tls.Config, error) {
	switch pin.Kind {
	case TrustPinCA:
		ca, err := parsePEMCertificate(pin.PEM)
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		roots.AddCert(ca)
		// Ordinary verification against a root set of exactly one: the chain
		// must reach the pinned CA, and the certificate must name the host
		// (an IP endpoint needs an IP SAN, as with any root).
		return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, nil
	case TrustPinLeafSPKI:
		want := pin.SHA256
		return &tls.Config{
			MinVersion: tls.VersionTLS12,
			// No chain is built, because there is no CA to build it to. The
			// pinned key is the whole identity, checked below along with the
			// certificate's validity window.
			InsecureSkipVerify: true, //nolint:gosec // Replaced by the exact SPKI match in VerifyConnection.
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 {
					return errors.New("upstream presented no certificate")
				}
				leaf := state.PeerCertificates[0]
				if SPKISHA256(leaf) != want {
					return &pinMismatchError{want: want, got: SPKISHA256(leaf)}
				}
				now := time.Now()
				if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
					return x509.CertificateInvalidError{Cert: leaf, Reason: x509.Expired}
				}
				return nil
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown pin kind %q", pin.Kind)
	}
}

// pinMismatchError is a leaf whose key is not the one pinned: the server
// rotated it, or is not the server that was trusted.
type pinMismatchError struct{ want, got string }

func (e *pinMismatchError) Error() string {
	return fmt.Sprintf("upstream key %s does not match the pinned %s", e.got, e.want)
}

// untrustedCertificate reports whether err is the upstream's certificate
// being refused, as opposed to the upstream being unreachable.
func untrustedCertificate(err error) bool {
	var (
		verify    *tls.CertificateVerificationError
		authority x509.UnknownAuthorityError
		hostname  x509.HostnameError
		invalid   x509.CertificateInvalidError
		mismatch  *pinMismatchError
	)
	return errors.As(err, &verify) || errors.As(err, &authority) || errors.As(err, &hostname) ||
		errors.As(err, &invalid) || errors.As(err, &mismatch)
}

func (h *httpProxy) setTrusts(table *trustTable) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trusts = table
}

func (h *httpProxy) trustTable() *trustTable {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.trusts
}

// roundTrip sends every outbound request: on the client's pinned transport
// when it holds a trust for the endpoint, on the proxy's own otherwise. An
// upstream certificate that is refused either way is answered here, as a 502
// that names the host, instead of the connection being dropped.
func (h *httpProxy) roundTrip(req *http.Request, ctx *goproxy.ProxyCtx) (*http.Response, error) {
	meta, _ := ctx.UserData.(*requestMeta)
	transport := ctx.Proxy.Tr
	if meta != nil && meta.trust != nil {
		transport = meta.trust.transport
	}
	resp, err := transport.RoundTrip(req)
	switch {
	case err == nil || meta == nil:
		return resp, err
	case untrustedCertificate(err):
		return h.refuseUntrusted(req, meta, err), nil
	default:
		return upstreamFailed(req, transport, err), nil
	}
}

// upstreamFailed answers a request the upstream never answered: the host was
// unreachable, or it — or the upstream proxy between this proxy and it — closed
// the connection. goproxy would drop the client's connection, which a sandbox
// sees as an empty reply and nothing else; a 502 that says what failed, and
// which upstream proxy it failed behind, is something an agent can act on.
//
// It is not a refusal. The request may have gone out and been acted on, so it
// is recorded by the response path as an ordinary exchange the upstream
// failed, as the gate's is.
func upstreamFailed(req *http.Request, transport *http.Transport, cause error) *http.Response {
	body := fmt.Sprintf("upstream failed: %s did not answer: %v\n", req.Host, cause)
	var via string
	if transport.Proxy != nil {
		if upstream, err := transport.Proxy(req); err == nil && upstream != nil {
			via = upstream.Redacted()
		}
	}
	if via != "" {
		body += fmt.Sprintf("This proxy reaches %s through the upstream proxy %s, which checks the host itself.\n"+
			"If that proxy refused the host's certificate, the host has to be trusted where that proxy runs.\n", req.Host, via)
	}
	resp := goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway, body)
	if via != "" {
		resp.Header.Set(UpstreamProxyHeader, via)
	}
	return resp
}

// refuseUntrusted answers a request whose upstream certificate was refused.
// Nothing was sent: the handshake is what failed.
func (h *httpProxy) refuseUntrusted(req *http.Request, meta *requestMeta, cause error) *http.Response {
	endpoint := requestEndpoint(req)
	reason := "upstream certificate not trusted"
	if meta.trust != nil {
		reason = "upstream certificate does not match the pin this sandbox trusts it by"
	}
	meta.answered = true
	meta.span.SetAttributes(attribute.Bool("proxy.blocked", true), attribute.Int("http.response.status_code", http.StatusBadGateway))
	h.audit.RecordHTTP(audit.HTTPEvent{
		Context:        meta.ctx,
		Time:           time.Now().UTC(),
		ClientID:       meta.client.ID,
		ClientSubject:  meta.client.Subject,
		ClientSerial:   meta.client.Serial,
		Method:         req.Method,
		URL:            meta.url(req),
		Host:           req.Host,
		Status:         http.StatusBadGateway,
		Blocked:        true,
		BlockedReason:  reason + ": " + cause.Error(),
		SwappedUseIDs:  meta.swappedUseIDs,
		RequestHeaders: req.Header,
	})
	meta.span.End()
	body := fmt.Sprintf("blocked by proxy: %s for %s (%v).\n"+
		"If this sandbox should reach %s, ask for it to be trusted:\n"+
		"  discobox-access trust %s --use \"what you will send it\" --why \"...\"\n",
		reason, endpoint, cause, endpoint, endpoint)
	resp := goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway, body)
	resp.Header.Set(UntrustedHostHeader, endpoint)
	return resp
}

// TLSProbe is what a host presented to this proxy's upstream path.
type TLSProbe struct {
	// Chain is the certificates the host sent, leaf first.
	Chain []*x509.Certificate
	// Verified reports whether Chain verifies for the host against the
	// system roots, which is what the proxy's upstream connections are held
	// to without a pin.
	Verified bool
	// Via is the upstream proxy the probe went out through, empty when it
	// reached the host directly. Behind an upstream that intercepts TLS — a
	// pool proxy running inside a discobox — Chain is that proxy's
	// certificate, not the host's: the host's own chain is only seen, and a
	// host only trusted, at the outermost proxy.
	Via string
}

// ErrProbeHostDenied is a probe for a host the client may not reach at all.
var ErrProbeHostDenied = errors.New("host denied for this client")

var errProbed = errors.New("probe complete")

// ProbeTLS connects to endpoint (host:port) the way clientID's traffic would
// leave — through the upstream proxy and its exemptions, when there is one —
// completes a TLS handshake with verification off, and returns the chain it
// was shown. It sends no request: the handshake is ended as soon as the chain
// is in hand. A host the allowlist refuses clientID is not probed.
func (s *Server) ProbeTLS(ctx context.Context, clientID, endpoint string) (TLSProbe, error) {
	endpoint = trustEndpoint(endpoint)
	if endpoint == "" {
		return TLSProbe{}, errors.New("probe needs a host:port")
	}
	flt, _ := s.http.policy()
	if !flt.AllowHostForClient(endpoint, clientID) {
		return TLSProbe{}, ErrProbeHostDenied
	}
	var (
		mu    sync.Mutex
		chain []*x509.Certificate
	)
	transport := s.http.proxy.Tr.Clone()
	transport.DisableKeepAlives = true
	transport.TLSClientConfig = &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // The chain is only recorded here, and verified below.
		VerifyConnection: func(state tls.ConnectionState) error {
			mu.Lock()
			chain = state.PeerCertificates
			mu.Unlock()
			return errProbed
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+endpoint+"/", nil)
	if err != nil {
		return TLSProbe{}, err
	}
	var via string
	if transport.Proxy != nil {
		if upstream, err := transport.Proxy(req); err == nil && upstream != nil {
			via = upstream.Redacted()
		}
	}
	resp, err := transport.RoundTrip(req)
	if resp != nil {
		// Unreachable in practice — the handshake is always ended before a
		// request is sent — but a response is a body to close whatever made it.
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chain) == 0 {
		if err == nil {
			err = errors.New("no certificate presented")
		}
		return TLSProbe{}, fmt.Errorf("connect to %s: %w", endpoint, err)
	}
	host, _, _ := net.SplitHostPort(endpoint)
	intermediates := x509.NewCertPool()
	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}
	_, verifyErr := chain[0].Verify(x509.VerifyOptions{DNSName: host, Intermediates: intermediates})
	return TLSProbe{Chain: chain, Verified: verifyErr == nil, Via: via}, nil
}
