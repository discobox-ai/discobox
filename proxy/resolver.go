package proxy

import "github.com/discobox-ai/discobox/proxy/internal/secrets"

// SecretResolver resolves a sentinel placeholder to its real credential value.
// Worker-agent implements it (calling the control plane); the proxy stays
// server-agnostic. A nil resolver passed to NewServer disables secret swapping.
type SecretResolver = secrets.Resolver

// SecretResolveRequest is the input to a SecretResolver.
type SecretResolveRequest = secrets.ResolveRequest

// SecretResolveResult is the output of a SecretResolver.
type SecretResolveResult = secrets.ResolveResult

// ErrSecretResolveDenied signals that a sentinel is unknown, unapproved, or not
// permitted for the requested host. The proxy leaves the sentinel in place.
var ErrSecretResolveDenied = secrets.ErrDenied
