package sandboxconfig

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// RuntimeConfig is the pool's whole view of a running sandbox: one document
// with a revision, which the sandbox agent takes at its runtime-config route
// and converges on (ADR 0126 §3). It replaces the files the pool used to stage
// into the sandbox's volumes after create — sandbox.json's idle timeout,
// secrets.json, the readiness marker, and the proxy material — with one
// delivery whose order is the document's own.
//
// It is whole rather than incremental: every field says what the sandbox is
// now, and a field that is absent is something the sandbox no longer has. A
// lost document is superseded by the next one; a lost delta would be drift.
//
// The wire shape is also the SandboxRuntimeConfig schema in
// api/openapi/server.yaml, which the sandbox agent's generated server decodes;
// the two are one contract, and a round-trip test in the sandbox agent holds
// them together.
type RuntimeConfig struct {
	// Revision orders documents. The sandbox applies a revision newer than the
	// one it holds, ignores an older one, and refuses a different document
	// under the revision it already applied. It starts at 1.
	Revision int64 `json:"revision"`
	// Agent is the part of sandbox.json the pool owns after create.
	Agent RuntimeAgent `json:"agent"`
	// SecretEnv is the sandbox's secret-bound environment, env name to
	// sentinel. Sentinels only: a resolved value never reaches a sandbox
	// (ADR 0012 §3).
	SecretEnv map[string]string `json:"secretEnv,omitempty"`
	// Proxy is the material for the sandbox's hop to its pool. Absent means
	// the sandbox has none.
	Proxy *RuntimeProxy `json:"proxy,omitempty"`
	// Sources says, per source, where its origin is, what is pinned, and
	// whether delivery has landed.
	Sources []RuntimeSource `json:"sources,omitempty"`
}

// RuntimeAgent is the sandbox-agent configuration the pool may change after
// create. It is applied into sandbox.json's agentRuntime, so it takes effect
// where that file is read: the idle timeout on the agent's next start, as it
// always has (ADR 0108 §3).
type RuntimeAgent struct {
	// IdleTimeout is the pool's idle timeout as a Go duration; empty leaves
	// the sandbox agent on its default.
	IdleTimeout string `json:"idleTimeout,omitempty"`
}

// RuntimeProxy is the proxy client material and trust a sandbox needs to reach
// its pool. The sandbox writes each piece where its readers already look
// (/etc/discobox/proxy), and renders the bridge configs with its own paths for
// them, since only the sandbox knows where it keeps them.
type RuntimeProxy struct {
	// MTLSCA is the PEM CA the pool's mTLS endpoints present certificates from.
	MTLSCA string `json:"mtlsCa"`
	// MITMCA is the PEM CA the egress proxy signs intercepted connections with,
	// which the sandbox adds to its trust store.
	MITMCA string `json:"mitmCa"`
	// ClientCert and ClientKey are this sandbox's PEM client keypair, its
	// identity on every hop to the pool. The key is delivered and never read
	// back: a document the sandbox keeps or answers with leaves it out.
	ClientCert string `json:"clientCert"`
	ClientKey  string `json:"clientKey,omitempty"`
	// Egress is the sandbox's loopback forwarder to the pool proxy, which also
	// carries the credentials endpoint and the DNS stub.
	Egress *RuntimeBridge `json:"egress,omitempty"`
	// NestedDocker is the forwarder for containers the sandbox's own dockerd
	// creates. It names no listen address: the sandbox discovers its bridge.
	NestedDocker *RuntimeBridge `json:"nestedDocker,omitempty"`
	// BuildKit is the forwarder to the pool's BuildKit mediator (ADR 0044).
	BuildKit *RuntimeBridge `json:"buildkit,omitempty"`
	// RegistryNamespace is the sandbox's namespace in the pool build registry.
	RegistryNamespace string `json:"registryNamespace,omitempty"`
}

// RuntimeBridge is one sandbox-side forwarder to the pool.
type RuntimeBridge struct {
	ListenAddress    string `json:"listenAddress,omitempty"`
	UpstreamURL      string `json:"upstreamUrl"`
	CredentialsURL   string `json:"credentialsUrl,omitempty"`
	DNSServer        string `json:"dnsServer,omitempty"`
	DNSListenAddress string `json:"dnsListenAddress,omitempty"`
}

// RuntimeSource is one of the sandbox's sources as the pool sees it. The
// sandbox carries and keeps these; cloning from them is not this document's
// job but the source convergence's (ADR 0126 §4).
type RuntimeSource struct {
	Slug string `json:"slug"`
	// OriginURL is where the source's origin is served, when it has one.
	OriginURL string `json:"originUrl,omitempty"`
	// Commit is the commit the source is pinned to.
	Commit string `json:"commit,omitempty"`
	// Delivered says the source is in place and the sandbox has settled on it:
	// the pool sets it only once it has also read the delivered project layer
	// and decided the sandbox's spec is final (ADR 0055).
	Delivered bool `json:"delivered,omitempty"`
}

// SourcesDelivered reports whether every source has been delivered, which is
// what the readiness marker (SourcesReadyFileName) says. A document naming no
// source has nothing to wait for.
func (c RuntimeConfig) SourcesDelivered() bool {
	for _, source := range c.Sources {
		if !source.Delivered {
			return false
		}
	}
	return true
}

// SameDocument reports whether two documents say the same thing, which is what
// decides whether a second delivery under one revision is a retry or a
// conflict. Absent and empty compare equal, as they read the same.
func (c RuntimeConfig) SameDocument(other RuntimeConfig) bool {
	a, errA := json.Marshal(c)
	b, errB := json.Marshal(other)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

// registryNamespacePattern is a single repository path component, which is all
// a registry namespace may be.
var registryNamespacePattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

// Validate refuses a document the sandbox could not apply whole. Everything a
// delivery can be wrong about is checked here, before anything is written, so
// that a refused document leaves the sandbox exactly as it was.
func (c RuntimeConfig) Validate() error {
	var errs []error
	if c.Revision < 1 {
		errs = append(errs, fmt.Errorf("revision must be at least 1, got %d", c.Revision))
	}
	if idle := c.Agent.IdleTimeout; idle != "" {
		if parsed, err := time.ParseDuration(idle); err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("agent.idleTimeout %q is not a positive duration", idle))
		}
	}
	for name := range c.SecretEnv {
		if name == "" || strings.ContainsAny(name, "=\x00") {
			errs = append(errs, fmt.Errorf("secretEnv name %q is not an environment variable name", name))
		}
	}
	if c.Proxy != nil {
		errs = append(errs, c.Proxy.validate())
	}
	seen := map[string]bool{}
	for _, source := range c.Sources {
		switch {
		case strings.TrimSpace(source.Slug) == "":
			errs = append(errs, errors.New("a source has no slug"))
		case seen[source.Slug]:
			errs = append(errs, fmt.Errorf("source %q is named twice", source.Slug))
		}
		seen[source.Slug] = true
		if source.OriginURL != "" {
			if _, err := url.Parse(source.OriginURL); err != nil {
				errs = append(errs, fmt.Errorf("source %q originUrl: %w", source.Slug, err))
			}
		}
	}
	return errors.Join(errs...)
}

func (p RuntimeProxy) validate() error {
	var errs []error
	for _, ca := range []struct{ name, pem string }{{"mtlsCa", p.MTLSCA}, {"mitmCa", p.MITMCA}} {
		if err := validateCertificates(ca.pem); err != nil {
			errs = append(errs, fmt.Errorf("proxy.%s: %w", ca.name, err))
		}
	}
	// A keypair that does not load is a hop that does not work, and the
	// readiness gate is held for exactly that hop: refuse it here rather than
	// let the forwarders find out.
	if _, err := tls.X509KeyPair([]byte(p.ClientCert), []byte(p.ClientKey)); err != nil {
		errs = append(errs, fmt.Errorf("proxy client keypair: %w", err))
	}
	for _, bridge := range []struct {
		name   string
		bridge *RuntimeBridge
	}{{"egress", p.Egress}, {"nestedDocker", p.NestedDocker}, {"buildkit", p.BuildKit}} {
		if bridge.bridge == nil {
			continue
		}
		if parsed, err := url.Parse(bridge.bridge.UpstreamURL); err != nil || parsed.Host == "" {
			errs = append(errs, fmt.Errorf("proxy.%s.upstreamUrl %q is not an absolute URL", bridge.name, bridge.bridge.UpstreamURL))
		}
	}
	if p.RegistryNamespace != "" && !registryNamespacePattern.MatchString(p.RegistryNamespace) {
		errs = append(errs, fmt.Errorf("proxy.registryNamespace %q is not a repository path component", p.RegistryNamespace))
	}
	return errors.Join(errs...)
}

// validateCertificates checks that text is one or more PEM certificates and
// nothing else.
func validateCertificates(text string) error {
	rest := []byte(text)
	var count int
	for {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return fmt.Errorf("PEM block %q is not a certificate", block.Type)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return err
		}
		count++
		rest = next
	}
	if count == 0 || strings.TrimSpace(string(rest)) != "" {
		return errors.New("not a PEM certificate bundle")
	}
	return nil
}
