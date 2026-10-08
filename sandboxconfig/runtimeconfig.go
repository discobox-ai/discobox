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

	"github.com/discobox-ai/discobox/sandboxpath"
)

// RuntimeConfig is the pool's whole view of a running sandbox: one document
// with a revision, which the sandbox agent takes at its runtime-config route
// and converges on (ADR 0126 §3). It is the dynamic half of what a sandbox is:
// everything that can change while it exists. The static half is the bootstrap,
// sandbox.json, which holds no private key and is never rewritten
// (ADR 26-10-08-127 §1).
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

// RuntimeConfigScope is the sandbox-agent token scope that reads and delivers
// a RuntimeConfig. It is the pool's alone: no wildcard grants it, and the pool
// signs it with its own key, which the sandbox's bootstrap names
// (PoolPublicKeyName; ADR 26-10-08-127 §3).
const RuntimeConfigScope = "runtime-config"

// RuntimeAgent is the sandbox-agent configuration the pool may change while
// the sandbox exists. It is applied to the running agent when delivered, not
// written into sandbox.json, which is the static bootstrap and is never
// rewritten (ADR 26-10-08-127 §§1, 5).
type RuntimeAgent struct {
	// IdleTimeout is the pool's idle timeout as a Go duration (ADR 0108);
	// empty leaves the sandbox agent on its default.
	IdleTimeout string `json:"idleTimeout,omitempty"`
}

// IdleTimeoutDuration is IdleTimeout parsed, zero when it is empty or does not
// parse — a document that names none leaves the sandbox on its default, and one
// that Validate would refuse is never applied.
func (a RuntimeAgent) IdleTimeoutDuration() time.Duration {
	parsed, err := time.ParseDuration(a.IdleTimeout)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

// RuntimeProxy is the credential and trust a sandbox needs on its hop to its
// pool. Where the pool is (Provider.Pool) is static and in the bootstrap, and
// where the sandbox listens is the sandbox's own; this is only what can change
// while the sandbox exists (ADR 26-10-08-127 §4). The sandbox writes each piece
// where its readers look (/etc/discobox/proxy) and renders its bridge configs
// from the three.
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
	// RegistryNamespace is the sandbox's namespace in the pool build registry.
	RegistryNamespace string `json:"registryNamespace,omitempty"`
}

// RuntimeSource is one of the sandbox's sources as the pool sees it: where its
// origin is, what is pinned, where it belongs and whether delivery has landed.
// The sandbox agent converges each one onto its target (ADR 0126 §4); how it
// is checked out — the branch, the upstream remote, a dirty-workspace snapshot
// — is the create-time placement in sandbox.json's Source of the same slug.
type RuntimeSource struct {
	Slug string `json:"slug"`
	// Target is the absolute in-sandbox path the source's checkout lives at.
	// Required with an OriginURL: a source with an origin is one the sandbox
	// clones, and it has to know where.
	Target string `json:"target,omitempty"`
	// OriginURL is where the source's origin is served, when it has one. The
	// sandbox clones from it and keeps it as the checkout's origin remote, so
	// a later `git fetch origin` reads the same place. It is the pool's
	// git-origins route for a local or pushed source, and the remote itself
	// for a remote-URL source.
	OriginURL string `json:"originUrl,omitempty"`
	// OriginToken is the bearer token OriginURL takes, when it takes one: the
	// pool's sandbox token, which fetches this sandbox's origins and nothing
	// else. The sandbox hands it to git through its credential helper, so it
	// is never written into the checkout. A newer document replaces it before
	// it expires.
	OriginToken string `json:"originToken,omitempty"`
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
// that a refused document leaves the sandbox exactly as it was. paths is the
// sandbox's own platform's path rules, which a source target is judged by
// (ADR 0145 §6): `C:\workspace\app` is a target on Windows and not on Linux.
func (c RuntimeConfig) Validate(paths sandboxpath.Paths) error {
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
		if source.Target != "" && (!paths.IsAbs(source.Target) || paths.Clean(source.Target) != source.Target) {
			errs = append(errs, fmt.Errorf("source %q target %q is not a clean absolute %s path", source.Slug, source.Target, pathsOS(paths)))
		}
		if source.OriginURL != "" {
			if _, err := url.Parse(source.OriginURL); err != nil {
				errs = append(errs, fmt.Errorf("source %q originUrl: %w", source.Slug, err))
			}
			if source.Target == "" {
				errs = append(errs, fmt.Errorf("source %q has an originUrl and no target", source.Slug))
			}
		}
		// The token reaches git as a header value through the credential
		// helper's line protocol, where a line break would be a second key.
		if strings.ContainsAny(source.OriginToken, "\r\n\x00") {
			errs = append(errs, fmt.Errorf("source %q originToken is not a single line", source.Slug))
		}
	}
	return errors.Join(errs...)
}

// pathsOS names the platform a path was judged for, in a refusal.
func pathsOS(paths sandboxpath.Paths) string {
	if paths.OS() == "" {
		return "POSIX"
	}
	return paths.OS()
}

func (p RuntimeProxy) validate() error {
	var errs []error
	for _, ca := range []struct{ name, pem string }{{"mtlsCa", p.MTLSCA}, {"mitmCa", p.MITMCA}} {
		if err := validateCertificates(ca.pem); err != nil {
			errs = append(errs, fmt.Errorf("proxy.%s: %w", ca.name, err))
		}
	}
	// A keypair that does not load is a hop that does not work: refuse it
	// here, before anything is written, rather than let the forwarders find
	// out.
	if _, err := tls.X509KeyPair([]byte(p.ClientCert), []byte(p.ClientKey)); err != nil {
		errs = append(errs, fmt.Errorf("proxy client keypair: %w", err))
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
