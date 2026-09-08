package registryauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

// failingKeychain stands in for Docker's keychain on a host whose credential
// helper cannot be run — the macOS keychain with no interactive session.
type failingKeychain struct {
	calls int
	err   error
}

func (k *failingKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	k.calls++
	return nil, k.err
}

type staticKeychain struct{ auth authn.Authenticator }

func (k staticKeychain) Resolve(authn.Resource) (authn.Authenticator, error) {
	return k.auth, nil
}

func resource(t *testing.T, image string) authn.Resource {
	t.Helper()
	ref, err := name.ParseReference(image)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	return ref.Context()
}

func TestResolveReturnsCredentialsWhenTheKeychainAnswers(t *testing.T) {
	want := authn.FromConfig(authn.AuthConfig{Username: "user", Password: "pass"})
	got, err := newKeychain(staticKeychain{auth: want}).ResolveContext(
		context.Background(), resource(t, "ghcr.io/discobox-ai/discobox-vm:latest"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != want {
		t.Fatalf("resolve returned %#v, want the keychain's own authenticator", got)
	}
}

// A credential helper that fails must not fail the read: the guest image is
// public, and the alternative is a pool that never boots.
func TestResolveFallsBackToAnonymousWhenTheKeychainFails(t *testing.T) {
	logs := captureLogs(t)
	base := &failingKeychain{err: errors.New(
		"error getting credentials - err: exit status 1, out: `keychain cannot be accessed`")}
	keychain := newKeychain(base)

	got, err := keychain.ResolveContext(context.Background(),
		resource(t, "ghcr.io/discobox-ai/discobox-vm:latest"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != authn.Anonymous {
		t.Fatalf("resolve returned %#v, want anonymous", got)
	}
	if !strings.Contains(logs.String(), "ghcr.io") {
		t.Fatalf("expected a warning naming the registry, got: %s", logs)
	}
}

// The caller is a reconciler that retries forever, so the warning is once per
// registry and every later resolve is silent.
func TestRepeatedFailuresWarnOncePerRegistry(t *testing.T) {
	logs := captureLogs(t)
	base := &failingKeychain{err: errors.New("helper is unhappy")}
	keychain := newKeychain(base)

	for range 3 {
		if _, err := keychain.ResolveContext(context.Background(),
			resource(t, "ghcr.io/discobox-ai/discobox-vm:latest")); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	}
	if _, err := keychain.ResolveContext(context.Background(),
		resource(t, "registry.example.test/team/image:latest")); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if base.calls != 4 {
		t.Fatalf("keychain consulted %d times, want 4", base.calls)
	}
	if warnings := strings.Count(logs.String(), "reading this registry anonymously"); warnings != 2 {
		t.Fatalf("logged %d warnings, want one per registry:\n%s", warnings, logs)
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return buf
}

// The end-to-end shape of the bug: Docker's own keychain, pointed at a config
// whose credential store cannot be run, fails the lookup outright — which is
// why this package wraps it. The process keychain reads anonymously instead.
func TestProcessKeychainSurvivesABrokenCredentialStore(t *testing.T) {
	captureLogs(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"credsStore":"discobox-no-such-helper"}`), 0o600); err != nil {
		t.Fatalf("write docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	target := resource(t, "ghcr.io/discobox-ai/discobox-vm:latest")

	if _, err := authn.DefaultKeychain.Resolve(target); err == nil {
		t.Skip("docker's keychain no longer fails on an unrunnable credential store")
	}
	got, err := Keychain().Resolve(target)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != authn.Anonymous {
		t.Fatalf("resolve returned %#v, want anonymous", got)
	}
}

// The private-image case: reading anonymously does not save that pull, so the
// credential failure has to reach the error the caller reports — every time,
// not once at whatever moment the store first failed to answer.
func TestExplainCarriesTheCredentialFailureIntoTheReadError(t *testing.T) {
	captureLogs(t)
	keychain := newKeychain(&failingKeychain{err: errors.New("keychain cannot be accessed")})
	ref, err := name.ParseReference("private.example.test/team/image:latest")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	unauthorized := errors.New("UNAUTHORIZED: authentication required")

	// Nothing has failed to resolve yet, so there is nothing to explain.
	if got := keychain.explain(ref, unauthorized); got.Error() != unauthorized.Error() {
		t.Fatalf("explain annotated %v before any degrade", got)
	}
	if _, err := keychain.ResolveContext(context.Background(), ref.Context()); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	got := keychain.explain(ref, unauthorized)
	if !errors.Is(got, unauthorized) {
		t.Fatalf("explain dropped the underlying error: %v", got)
	}
	for _, want := range []string{"keychain cannot be accessed", "private.example.test"} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("explain returned %q, want it to name %q", got, want)
		}
	}
	// A read against a registry that resolved fine is left alone.
	other, err := name.ParseReference("ghcr.io/discobox-ai/discobox-vm:latest")
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}
	if got := keychain.explain(other, unauthorized); got.Error() != unauthorized.Error() {
		t.Fatalf("explain annotated an error from an undegraded registry: %v", got)
	}
}

func TestExplainPassesThroughNoError(t *testing.T) {
	if got := newKeychain(staticKeychain{auth: authn.Anonymous}).explain(nil, nil); got != nil {
		t.Fatalf("explain(nil, nil) = %v, want nil", got)
	}
}
