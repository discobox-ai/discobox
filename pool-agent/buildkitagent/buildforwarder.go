package buildkitagent

import (
	"fmt"
	"strings"
)

// The per-build forwarder is what binds a build step's egress to the sandbox
// that asked for the build.
//
// A build step reaches the pool proxy the same way everything else in Discobox
// does: through a plaintext forwarder that holds an mTLS client certificate.
// What differs is where the forwarder listens. It binds inside that one build
// step's own network namespace, so the address is reachable only from that
// step. Nothing has to be secret — the identifier the mediator puts in the
// build's proxy URL is inert anywhere else, because the listener it names does
// not exist in any other namespace.
//
// That is the property ADR 0020 required before a pool-shared builder could
// exist at all: "a mechanism that binds each build's egress to its owning
// sandbox's client certificate". The OCI spec still carries no tenant
// identity of its own; the mediator supplies it, and this puts it to work.
//
// Running one takes namespace and signal calls that exist only on Linux, so
// that half lives in buildforwarder_linux.go. What stays here is the address
// and the identity encoded into it — the contract between the mediator and the
// runc wrapper, which both sides need to agree on wherever they are compiled.

// PoolProxyURL mirrors proxyagent.PoolProxyURL. It is duplicated rather than
// imported for the same reason the rest of this package duplicates it: the
// builder's wiring does not depend on the proxy's.
const PoolProxyURL = "https://" + RegistryServerName + ":17080"

// BuildProxyAddress is where a per-build forwarder listens, inside the build
// step's own network namespace. Everything that binds, injects, or recognizes
// that address derives it here: a listener and a matcher that disagreed would
// leave the build with a proxy variable pointing at nothing.
func BuildProxyAddress() string {
	return fmt.Sprintf("127.0.0.1:%d", BuildForwarderPort)
}

// BuildProxyURL is the proxy address injected into one sandbox's builds. The
// sandbox ID rides as userinfo so the runc wrapper can read back which
// sandbox's certificate the forwarder should present.
//
// It is safe in the clear. The listener lives in the build step's own network
// namespace, so this address resolves to nothing anywhere else, and the string
// grants nothing on its own.
func BuildProxyURL(sandboxID string) string {
	return fmt.Sprintf("http://%s@%s", sandboxID, BuildProxyAddress())
}

// SandboxFromProxyURL recovers the sandbox ID from a BuildProxyURL value, and
// returns "" for anything else — including a proxy address a user set
// themselves, which must never be mistaken for an identity.
func SandboxFromProxyURL(value string) string {
	const scheme = "http://"
	rest, ok := strings.CutPrefix(value, scheme)
	if !ok {
		return ""
	}
	id, addr, ok := strings.Cut(rest, "@")
	if !ok || id == "" {
		return ""
	}
	if addr != BuildProxyAddress() {
		return ""
	}
	return id
}

// StripProxyEnv is the runc wrapper's spec-editing rule: for any variable that
// can carry the build's proxy address, return the value the container should
// see instead.
//
// It lives beside BuildProxyURL rather than in the wrapper because the set of
// variables that carry the identity is decided here, by whatever the mediator
// injects. Splitting the two is how HTTPS_PROXY came to be left untouched while
// HTTP_PROXY was cleaned.
func StripProxyEnv(name, value string) string {
	switch strings.ToUpper(name) {
	case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
		return StripProxyIdentity(value)
	}
	return value
}

// StripProxyIdentity removes the userinfo before the container sees the value.
// The build itself has no use for it, and leaving it in would put the owning
// sandbox's ID in every RUN step's environment.
func StripProxyIdentity(value string) string {
	if SandboxFromProxyURL(value) == "" {
		return value
	}
	return "http://" + BuildProxyAddress()
}
