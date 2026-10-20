package provision

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PlaceholderDir is where a feature provisioning in the background has its
// commands stand in (ADR 26-10-16-423 §5). It is last on every PATH, so the
// real command shadows its placeholder as soon as the feature has installed
// it, and only a command not there yet reaches one.
const PlaceholderDir = "/usr/local/lib/discobox/placeholders"

// PlaceholderProfile puts PlaceholderDir last on a login shell's PATH. The
// manifest's PATH carries it for every other exec.
const PlaceholderProfile = "/etc/profile.d/zz-discobox-placeholders.sh"

// ProvisionCommand is the agent's discobox-provision personality, which a
// placeholder runs.
const ProvisionCommand = "/usr/local/bin/discobox-provision"

// writePlaceholders replaces the placeholders with those of the features
// provisioning in the background now: on, not adopted, and naming commands.
// One left by an earlier boot for a feature that is ready or off goes.
func (r *Runner) writePlaceholders(plans []*featurePlan, opts Options) error {
	if r.Placeholders == "" {
		return nil
	}
	dir := r.path(r.Placeholders)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("placeholders: %w", err)
	}
	written := false
	for _, plan := range plans {
		if !opts.Background || plan.off != "" || plan.adopted || plan.failed != nil {
			continue
		}
		for _, command := range plan.feature.Spec.Commands {
			script := fmt.Sprintf("#!/bin/sh\n"+
				"# Written by the sandbox agent: %[2]s stands here while the %[1]s feature\n"+
				"# is provisioned, and is removed once it is ready (ADR 26-10-16-423 §5).\n"+
				"exec %[3]s run %[1]s %[2]s \"$@\"\n", plan.feature.Name, command, ProvisionCommand)
			if err := writeFileAtomic(filepath.Join(dir, command), []byte(script), 0o755); err != nil {
				return fmt.Errorf("placeholders: %w", err)
			}
			written = true
		}
	}
	if !written {
		return nil
	}
	profile := fmt.Sprintf("# shellcheck shell=sh # Sourced by /etc/profile, never run.\n"+
		"# Written by the sandbox agent: placeholders for commands still being\n"+
		"# provisioned, last on PATH so the real command shadows each one.\n"+
		"case \":${PATH}:\" in *:%[1]s:*) ;; *) PATH=\"${PATH}\":%[1]s ;; esac\nexport PATH\n", shellQuote(r.Placeholders))
	if err := writeFileAtomic(r.path(PlaceholderProfile), []byte(profile), 0o644); err != nil {
		return fmt.Errorf("placeholders: %w", err)
	}
	return nil
}

// removePlaceholders removes a ready feature's placeholders.
func (r *Runner) removePlaceholders(plan *featurePlan) error {
	if r.Placeholders == "" {
		return nil
	}
	for _, command := range plan.feature.Spec.Commands {
		if err := os.Remove(r.path(filepath.Join(r.Placeholders, command))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
