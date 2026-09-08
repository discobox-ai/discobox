// Package registryauth resolves credentials for the reads this server makes
// directly against an OCI registry: the guest image a VM pool boots
// (server/providers/guestimage) and the harness images a project seeds
// (server/internal/resources/harnessconfigs).
//
// It is Docker's keychain with one difference: a credential source that fails
// is treated as no credentials rather than as a failed read. Discobox's own
// images are public, so a broken credential store must not be what stops a
// pool from booting.
//
// The failure this exists for is macOS. A `credsStore` in ~/.docker/config.json
// makes every registry lookup — including one for a registry the user never
// logged in to — shell out to a helper, and docker-credential-osxkeychain
// fails outright rather than reporting "no credentials" when the process has no
// interactive security session:
//
//	error getting credentials - err: exit status 1, out: `keychain cannot be
//	accessed because the current session does not allow user interaction`
//
// The local server is started detached, and is commonly started from an SSH
// session or a headless Mac, so it is exactly that kind of process. Docker's
// keychain returns that error, go-containerregistry fails the fetch with it,
// and the pool reconciler retries the same failure forever — for an image that
// needs no credentials at all. Unlocking the keychain does not help: the error
// is about the session, not about the lock.
//
// Reading anonymously is not free for an image that genuinely needs
// credentials, which a harness image named by a project may well be. That read
// fails at the registry instead, as an unauthorized — and it goes on failing
// every time the reconciler tries again, long after the one warning about the
// credential store scrolled out of the log. So the credential failure is
// carried rather than only logged: Explain puts it into the error the caller
// reports, and the diagnosis arrives with every retry of the failure it
// explains.
package registryauth

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// shared is the process's keychain. One instance, so the warning about a
// credential store that is not answering is logged once rather than once per
// reconcile pass, and so a read that fails knows about a degrade that happened
// on some earlier pass.
var shared = newKeychain(authn.DefaultKeychain)

// Keychain returns the keychain every registry read in this server uses.
func Keychain() authn.Keychain { return shared }

// Explain annotates an error from reading ref with the credential failure that
// made that read anonymous, when there was one, and returns it unchanged when
// there was not.
//
// This is what keeps a broken credential store diagnosable for an image that
// does need credentials. Without it the operator sees "UNAUTHORIZED" on every
// retry and the reason sits in a single warning from whenever the store first
// failed to answer.
func Explain(ref name.Reference, err error) error { return shared.explain(ref, err) }

type keychain struct {
	base authn.Keychain

	mu sync.Mutex
	// degraded maps a registry to the credential error that made this process
	// read it anonymously.
	degraded map[string]error
}

func newKeychain(base authn.Keychain) *keychain {
	return &keychain{base: base, degraded: map[string]error{}}
}

// Resolve implements authn.Keychain.
func (k *keychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	return k.ResolveContext(context.Background(), target)
}

// ResolveContext implements authn.ContextKeychain, which is what
// go-containerregistry calls when it has a context to pass.
func (k *keychain) ResolveContext(ctx context.Context, target authn.Resource) (authn.Authenticator, error) {
	auth, err := authn.Resolve(ctx, k.base, target)
	if err == nil {
		return auth, nil
	}
	// Anonymous rather than the error: a public image still pulls, and a
	// private one fails at the registry with an unauthorized that Explain
	// annotates with this. Both outcomes are better than refusing to read
	// anything because a credential helper is unhappy.
	k.degrade(ctx, target.RegistryStr(), err)
	return authn.Anonymous, nil
}

// degrade records a registry this process is reading anonymously, and warns the
// first time for each one.
//
// Warning once, because the caller is a reconciler: this is reached again on
// every retry, and a warning per attempt would bury the failure it is trying to
// explain. What that would otherwise cost — the reason being gone from the log
// by the time a repeated failure is looked at — is paid for by Explain instead,
// which is why the error is kept and not just logged.
func (k *keychain) degrade(ctx context.Context, registry string, err error) {
	k.mu.Lock()
	_, seen := k.degraded[registry]
	k.degraded[registry] = err
	k.mu.Unlock()
	if seen {
		return
	}
	slog.WarnContext(ctx, "docker credentials could not be read; reading this registry anonymously",
		"registry", registry,
		"err", err,
		"hint", "public images still work; a private one will fail as unauthorized. Check the credsStore/credHelpers entries in ~/.docker/config.json.")
}

func (k *keychain) explain(ref name.Reference, err error) error {
	if err == nil || ref == nil {
		return err
	}
	registry := ref.Context().RegistryStr()
	k.mu.Lock()
	credentialErr, degraded := k.degraded[registry]
	k.mu.Unlock()
	if !degraded {
		return err
	}
	// Both wrapped: the caller's errors.Is still finds the registry's own
	// error, and the credential failure travels with it rather than as text.
	return fmt.Errorf("%w (read anonymously: docker credentials for %s could not be read: %w)",
		err, registry, credentialErr)
}
