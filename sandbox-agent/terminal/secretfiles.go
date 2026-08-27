package terminal

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// A harness that authenticates from a file reads a sentinel the proxy swaps on
// the way out (harness.SecretDeliveryFile). The file is installed once, at
// terminal create and revive, because the sentinel does not expire and nothing
// in the sandbox has a reason to rewrite it.
//
// A harness does have a reason: an upstream 401. Claude Code reads a 401 as an
// expired login and rewrites ~/.claude/.credentials.json with the credential
// fields emptied, keeping the rest. It then tries to refresh, which cannot
// work — the refresh token it was handed is a placeholder, because refreshing
// happens in the control plane. So one 401 the sandbox did nothing to cause (a
// rotated access token, a blip in resolution) logs the harness out for the life
// of the sandbox, while the credential the file pointed at is still good.
//
// Restoring the file is what makes that recoverable: the sentinel is durable,
// so re-rendering the template puts the harness back where it started. It does
// not un-log-out a running process — Claude Code caches that decision until it
// restarts — but it makes the next launch a signed-in one instead of leaving
// every launch after the first 401 logged out.
//
// The invariant is narrow on purpose: a templated file that rendered a sentinel
// must still contain it. Nothing else about the file is reconciled, so a
// harness that legitimately rewrote its own settings keeps them.

// secretFilesInterval is how often the files are re-checked. Latency does not
// matter the way it does for a config watcher: the harness that cleared the
// file has already cached its own logged-out state, so what this loop restores
// is the *next* launch, not the running one.
const secretFilesInterval = 30 * time.Second

// RestoreSecretFiles re-installs any of this harness's files whose delivered
// sentinel has gone missing from the file on disk, returning the paths it
// restored.
func (s *Service) RestoreSecretFiles(ctx context.Context) ([]string, error) {
	return s.installer.RestoreSecretFiles(ctx, s.harness, s.installEnv())
}

// installEnv is the environment the file installer resolves a home directory
// against: the same base a terminal is created with, minus anything a specific
// create request would have added. Home is all it reads it for.
func (s *Service) installEnv() map[string]string {
	base := s.env
	if s.secretEnv != nil {
		base = execs.MergeEnv(base, s.exportedSecretEnv())
	}
	return execs.EnvWithRuntimeDefaults(base, s.defaultUser)
}

// WatchSecretFiles keeps the harness's sentinel-bearing files intact for as
// long as ctx lives. It returns when ctx is done.
func (s *Service) WatchSecretFiles(ctx context.Context, logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	if len(s.harness.Files) == 0 {
		return
	}
	// Not in a config sandbox. There, the credentials file is not a delivery to
	// be kept intact — it is the flow's output: the user logs in inside the
	// session and the configure script reads the real credential back out of
	// that file to capture it (harness/claude-code/configure.sh,
	// extract_oauth_payload). Restoring the sentinel over it would overwrite
	// the login being captured, breaking the one flow that produces a
	// credential in the first place.
	if s.harnessMode == configHarnessMode {
		return
	}
	ticker := time.NewTicker(secretFilesInterval)
	defer ticker.Stop()
	for {
		restored, err := s.RestoreSecretFiles(ctx)
		if err != nil {
			logger.Warn("restore harness secret files", "error", err)
		}
		for _, path := range restored {
			// Not necessarily "cleared": at boot this also covers the file not
			// being installed yet, which is the same write either way.
			logger.Info("restored harness credential file", "path", path)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RestoreSecretFiles re-renders every templated file whose rendered content
// carries a sentinel that the file on disk no longer contains.
//
// createOnly files are left alone. That flag says the sandbox owns the file
// after its first write, and restoring one would take a file back from the
// harness it was handed to. A credential delivered that way is delivered once
// by contract; reconciling it would break the contract rather than the bug.
func (i FileInstaller) RestoreSecretFiles(_ context.Context, harness config.Harness, env map[string]string) ([]string, error) {
	if len(harness.Files) == 0 || i.Secrets == nil {
		return nil, nil
	}
	sentinels := i.Secrets()
	if len(sentinels) == 0 {
		return nil, nil
	}
	home, err := resolveHomeDir(i.HomeDirectory, i.User, env)
	if err != nil {
		return nil, fmt.Errorf("harness %q has files to restore but %w", harness.ID, err)
	}
	home = filepath.Clean(home)

	var restored []string
	for _, file := range harness.Files {
		if !file.Template || file.CreateOnly {
			continue
		}
		path, err := homeRelativePath(home, file.Path)
		if err != nil {
			return restored, fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
		content, err := renderHarnessFileTemplate(file.Path, file.Content, i.templateContext())
		if err != nil {
			return restored, fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
		delivered := deliveredSentinels(content, sentinels)
		if len(delivered) == 0 {
			continue
		}
		onDisk, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return restored, fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
		if err == nil && containsAll(string(onDisk), delivered) {
			continue
		}
		if err := writeHarnessFile(path, content, false, i.uid(), i.gid()); err != nil {
			return restored, fmt.Errorf("harness %q file %q: %w", harness.ID, file.Path, err)
		}
		restored = append(restored, file.Path)
	}
	return restored, nil
}

// deliveredSentinels is the set of sentinel values a rendered file actually
// carries. A file that renders none is not a credential delivery and is never
// restored.
func deliveredSentinels(rendered string, sentinels map[string]string) []string {
	var out []string
	for _, sentinel := range sentinels {
		if sentinel == "" || !strings.Contains(rendered, sentinel) {
			continue
		}
		if !slices.Contains(out, sentinel) {
			out = append(out, sentinel)
		}
	}
	return out
}

func containsAll(content string, values []string) bool {
	for _, value := range values {
		if !strings.Contains(content, value) {
			return false
		}
	}
	return true
}
