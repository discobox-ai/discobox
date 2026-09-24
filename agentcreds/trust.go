package agentcreds

import (
	"net"
	"strings"
	"time"
)

// Host trust: the protocol's second verb (ADR 0149). An agent asks for a host
// whose certificate the implementation's egress does not trust to be trusted
// for its sandbox, and a human pins one certificate from the chain the
// implementation observed. Nothing is handed to a process: a trust changes what
// the implementation's egress accepts, for the whole sandbox.

// Trust route paths, relative to the configured base URL.
const (
	// PathTrusts lists the host trusts the caller holds.
	PathTrusts = "/" + Version + "/trusts"
	// PathTrustRequests creates a trust request; a request ID appended to it
	// reads that request's status.
	PathTrustRequests = "/" + Version + "/trusts/requests"
)

// StatusUnneeded settles a trust request that asked for nothing: the host's
// chain already verifies against the implementation's ordinary roots, so no
// human is asked and nothing is pinned. It is terminal, like granted and
// denied.
const StatusUnneeded = "unneeded"

// Pin kinds. A pin is one certificate from the observed chain, never a
// setting that turns verification off.
const (
	// PinCA pins a CA certificate: the chain must verify to it, and the name
	// is checked as usual.
	PinCA = "ca"
	// PinLeafSPKI pins the server certificate's public key, for a server that
	// presents no CA in its chain.
	PinLeafSPKI = "leaf-spki"
)

// Certificate is one certificate as the implementation's egress observed it.
// It carries what a person needs to decide, never the private half of
// anything.
type Certificate struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	DNSNames  []string  `json:"dnsNames,omitempty"`
	IPs       []string  `json:"ips,omitempty"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
	// SHA256 is the hex SHA-256 of the certificate's DER, what a PinCA names.
	SHA256 string `json:"sha256"`
	// SPKISHA256 is the hex SHA-256 of its SubjectPublicKeyInfo, what a
	// PinLeafSPKI names.
	SPKISHA256 string `json:"spkiSha256"`
	IsCA       bool   `json:"isCA,omitempty"`
	SelfSigned bool   `json:"selfSigned,omitempty"`
	// PEM is the certificate itself, public material, so the chain an
	// implementation observed can be carried whole to whoever pins from it.
	PEM string `json:"pem,omitempty"`
}

// Pin names the certificate a trust verifies a host against.
type Pin struct {
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
}

// TrustRequestBody asks a human to trust a host for the caller's sandbox.
//
// Host is host:port; a host with no port means 443. SuppliedCA is an optional
// PEM CA certificate the caller already has from a source it trusts — a cloud
// API that describes the cluster, say. It is only a suggestion: the request is
// refused unless the chain the implementation observes verifies against it.
//
// GrantTTLSeconds is read as a credential request's is: the approver's
// default, never a term, from 1 to MaxGrantTTLSeconds.
type TrustRequestBody struct {
	Host            string         `json:"host"`
	Justification   string         `json:"justification,omitempty"`
	Uses            []RequestedUse `json:"uses,omitempty"`
	SuppliedCA      string         `json:"suppliedCA,omitempty"`
	GrantTTLSeconds int64          `json:"grantTTLSeconds,omitempty"`
}

// TrustRequestStatus is what a trust request and its poll both answer with.
//
// ObservedChain is the chain the implementation's egress was shown, leaf
// first. Reason says why a request settled without a human (StatusUnneeded).
// Pin and Uses are present once granted; the granted uses are authoritative
// over the requested ones.
type TrustRequestStatus struct {
	RequestID     string        `json:"requestId"`
	Status        string        `json:"status"`
	Host          string        `json:"host"`
	Reason        string        `json:"reason,omitempty"`
	ObservedChain []Certificate `json:"observedChain,omitempty"`
	Pin           *Pin          `json:"pin,omitempty"`
	Uses          []Use         `json:"uses,omitempty"`
}

// Settled reports whether a trust request has reached a terminal status.
func (s TrustRequestStatus) Settled() bool {
	return s.Status == StatusGranted || s.Status == StatusDenied || s.Status == StatusUnneeded
}

// Trust is one host the caller's sandbox trusts, and the uses it was trusted
// for. Every request the sandbox sends to Host is judged against them.
type Trust struct {
	Host      string     `json:"host"`
	Pin       Pin        `json:"pin"`
	Uses      []Use      `json:"uses,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// TrustListResponse is the trust list operation's body.
type TrustListResponse struct {
	Trusts []Trust `json:"trusts"`
}

// TrustHost normalizes a host a trust is asked for or matched against:
// lowercased, trimmed, with :443 filled in when no port is named. A trust
// covers exactly one endpoint, so two spellings of it must be one string. It
// returns "" for a value that names no host.
func TrustHost(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.TrimPrefix(value, "https://")
	value = strings.TrimSuffix(value, "/")
	if value == "" || strings.ContainsAny(value, "/?#@ ") {
		return ""
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		// No port, or an IPv6 literal written without brackets.
		host, port = strings.Trim(value, "[]"), "443"
	}
	if host == "" || port == "" {
		return ""
	}
	return net.JoinHostPort(host, port)
}
