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
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/sandboxpath"
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
	// RawSecrets are the sandbox's raw secrets, variable to RawDeliveryEnv or
	// RawDeliveryFile (ADR 26-10-27-840). Never a value: the sandbox asks its
	// pool for those (RawSecretsRoute) when this changes, and while it holds
	// any. A pool sends it only to a sandbox whose status advertised raw
	// secrets, since an older agent refuses a document with a field it does
	// not know.
	RawSecrets map[string]string `json:"rawSecrets,omitempty"`
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

// AutostopConfirmScope is the sandbox-agent token scope that confirms a
// power-down the pool is about to carry out (ADR 26-10-16-337 §5), and that
// tells the sandbox the pool thawed it (ADR 26-10-16-420 §4). Like
// RuntimeConfigScope it is the pool's alone and signed with the pool's key:
// the pool holds the power lock the confirmation is asked under.
const AutostopConfirmScope = "autostop-confirm"

// CacheClearScope is the sandbox-agent token scope that empties the sandbox's
// caches in place (ADR 26-10-23-551 §3): a machine sandbox's cache is on its
// own disk, so its pool clears it through its agent. Like the scopes above it
// is the pool's alone and signed with the pool's key.
const CacheClearScope = "cache-clear"

// RuntimeAgent is the sandbox-agent configuration the pool may change while
// the sandbox exists. It is applied to the running agent when delivered, not
// written into sandbox.json, which is the static bootstrap and is rewritten
// only when its box is rebuilt: a container replaced, or a machine made again
// in place (ADR 26-10-08-127 §§1, 5, ADR 26-10-27-345).
type RuntimeAgent struct {
	// IdleTimeout is the pool's idle timeout as a Go duration (ADR 0108);
	// empty leaves the sandbox agent on its default.
	IdleTimeout string `json:"idleTimeout,omitempty"`
	// ControlPlanePower hands the sandbox's power to the control plane: the
	// agent reports idle, sleep and wake on its status and no longer powers
	// itself off (ADR 26-10-16-337 §8). The pool sets it only for a sandbox
	// whose status advertised it, because an agent that predates it refuses a
	// whole delivery over a field it does not know.
	ControlPlanePower bool `json:"controlPlanePower,omitempty"`
	// TLS renews the keypair the agent serves its API with. The pool sets it
	// only for a sandbox whose bootstrap told its agent to serve TLS
	// (AgentRuntime.TLS), which is new enough to know the field; absent leaves
	// the keypair the agent holds.
	TLS *AgentTLS `json:"tls,omitempty"`
}

// AgentTLS is the keypair a sandbox's agent serves its API over TLS with,
// which its pool's mTLS CA signed for the sandbox's private name
// (AgentServerName), so the pool knows which sandbox answered (ADR
// 26-10-29-481 §3). The pool places it in the config volume (AgentTLSDirName)
// before the agent starts, at every start, and renews it
// in the runtime-config document. The key is delivered and never read back: a
// document the sandbox keeps or answers with leaves it out.
type AgentTLS struct {
	Cert string `json:"cert"`
	Key  string `json:"key,omitempty"`
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
}

// RuntimeSource is one of the sandbox's sources as the pool sees it: where its
// origin is, what is pinned, where it belongs and whether delivery has landed.
// The sandbox agent converges each one onto its target (ADR 0126 §4); how it
// is checked out — the branch, the upstream remote, a dirty-workspace snapshot
// — is the create-time placement in sandbox.json's Source of the same slug.
type RuntimeSource struct {
	Slug string `json:"slug"`
	// Target is the in-sandbox path the source's checkout lives at: absolute,
	// or home-relative for a skills source (ADR 26-11-01-692), which the
	// sandbox expands (ExpandHome). Required with an OriginURL: a source with an origin is one the sandbox
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

// WithoutPrivateKeys is the document with the sandbox's private keys taken
// out — its client key and its agent's TLS key: what the pool records and the
// sandbox keeps or answers with, none of which needs them. Each key's one copy
// is where its reader reads it, or the pool's own material.
func (c RuntimeConfig) WithoutPrivateKeys() RuntimeConfig {
	if c.Proxy != nil {
		proxy := *c.Proxy
		proxy.ClientKey = ""
		c.Proxy = &proxy
	}
	if c.Agent.TLS != nil {
		keypair := *c.Agent.TLS
		keypair.Key = ""
		c.Agent.TLS = &keypair
	}
	return c
}

// SameDocument reports whether two documents say the same thing, which is what
// decides whether a second delivery under one revision is a retry or a
// conflict. Absent and empty compare equal, as they read the same.
func (c RuntimeConfig) SameDocument(other RuntimeConfig) bool {
	a, errA := json.Marshal(c)
	b, errB := json.Marshal(other)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

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
	for name, delivery := range c.RawSecrets {
		switch {
		case name == "" || strings.ContainsAny(name, "=\x00/") || name == "." || name == "..":
			errs = append(errs, fmt.Errorf("rawSecrets name %q is not an environment variable name", name))
		case !ValidRawDelivery(delivery):
			errs = append(errs, fmt.Errorf("rawSecrets %s is delivered %q, not %q or %q", name, delivery, RawDeliveryEnv, RawDeliveryFile))
		}
	}
	if c.Proxy != nil {
		errs = append(errs, c.Proxy.validate())
	}
	if c.Agent.TLS != nil {
		if _, err := tls.X509KeyPair([]byte(c.Agent.TLS.Cert), []byte(c.Agent.TLS.Key)); err != nil {
			errs = append(errs, fmt.Errorf("agent tls keypair: %w", err))
		}
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
		if source.Target != "" && !paths.IsTarget(source.Target) {
			errs = append(errs, fmt.Errorf("source %q target %q is neither a clean absolute %s path nor a home-relative one", source.Slug, source.Target, pathsOS(paths)))
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

// ExpandHome is the document with each home-relative source target
// (sandboxpath.HomeRelative) joined onto home, which only the sandbox knows
// (ADR 26-11-01-692 §3). The sandbox agent converges on what this returns and
// keeps the document as delivered, so a retried delivery still compares equal
// to the one held.
func (c RuntimeConfig) ExpandHome(paths sandboxpath.Paths, home string) RuntimeConfig {
	if len(c.Sources) > 0 {
		sources := make([]RuntimeSource, len(c.Sources))
		for i, source := range c.Sources {
			source.Target = paths.ExpandHome(source.Target, home)
			sources[i] = source
		}
		c.Sources = sources
	}
	return c
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
