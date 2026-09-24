package libkrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/discobox-ai/discobox/health"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

// CheckHost reports whether this host can run the libkrun provider a first
// start would install as its default, and a *sandbox.ProviderUnavailableError
// saying why when it cannot (ADR 0148 §2).
//
// It asks what a pool's boot otherwise discovers, in the order a boot would:
// the platform, KVM, then the runtime the libkrun image carries — fetched, as
// the first pool would fetch it, and loaded in a launcher child, because the
// server never maps libkrun itself (ADR 0062 §9). A first start is the one
// moment these are the server's to answer. Once a provider is installed they
// are its pools' failures, reported where a pool reports them.
//
// An image that cannot be fetched is not one of them. A registry that is down
// or a network that is not up yet says nothing about this host, and offering
// Docker for it would trade the VM boundary away for good over a failure that
// retrying fixes. It is returned as the plain error it is, and the start fails
// with it.
func CheckHost(ctx context.Context, defaults dockerworker.ServerDefaults) error {
	if err := krunvm.CheckKVM(); err != nil {
		reason := health.ReasonKVMUnavailable
		if errors.Is(err, krunvm.ErrUnsupported) {
			reason = health.ReasonArchUnsupported
		}
		return &sandbox.ProviderUnavailableError{Provider: ProviderType, Reason: reason, Err: err}
	}
	resolver, err := imageResolver(Config{}, defaults)
	if err != nil {
		return err
	}
	image, err := resolver.Resolve(ctx, nil)
	if err != nil {
		return fmt.Errorf("fetch the libkrun image to check this host can run it: %w", err)
	}
	if err := checkLibrary(ctx, image.Path(libraryArtifact)); err != nil {
		return &sandbox.ProviderUnavailableError{Provider: ProviderType, Reason: health.ReasonRuntimeUnloadable, Err: err}
	}
	return nil
}

// checkLibrary loads libkrun in a launcher child and reports what it said if
// it could not, which already names the library and why.
func checkLibrary(ctx context.Context, path string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this server binary: %w", err)
	}
	//nolint:gosec // The command is this binary and the path is one the resolver produced.
	output, err := exec.CommandContext(ctx, self, launcherCommand, checkLibraryFlag, path).CombinedOutput()
	if err != nil {
		if message := strings.TrimSpace(strings.TrimPrefix(string(output), launcherErrorPrefix)); message != "" {
			return errors.New(message)
		}
		return fmt.Errorf("load %s: %w", path, err)
	}
	return nil
}
