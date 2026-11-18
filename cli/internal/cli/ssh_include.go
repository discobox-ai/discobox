package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

const sshIncludeHint = "Run discobox admin ssh-config enable to add Discobox's SSH aliases to your SSH config."

type sshIncludePrompt func(context.Context) (sshIncludePreference, error)

func inlineSSHIncludePrompt(cmd *cobra.Command, status *statusLine) sshIncludePrompt {
	if !canAskInline(cmd) {
		hint := sync.OnceFunc(func() { fmt.Fprintln(cmd.ErrOrStderr(), sshIncludeHint) })
		return func(context.Context) (sshIncludePreference, error) {
			hint()
			return "", nil
		}
	}
	return func(context.Context) (sshIncludePreference, error) {
		defer status.suspend()()
		choice, err := pickOne(cmd, "Add Discobox Include lines to your SSH config (~/.ssh/config and Windows SSH config when running under WSL)?", []pickerItem{
			{id: "no", title: "No"},
			{id: "yes", title: "Yes, enable SSH aliases"},
		}, pickerOptions{})
		if errors.Is(err, errPickCanceled) && cmd.Context().Err() == nil {
			return sshIncludeDisabled, nil
		}
		return sshIncludeAnswer(choice == "yes"), err
	}
}

type sshIncludePreference string

const (
	sshIncludeEnabled  sshIncludePreference = "enabled"
	sshIncludeDisabled sshIncludePreference = "disabled"
)

func sshIncludeAnswer(yes bool) sshIncludePreference {
	if yes {
		return sshIncludeEnabled
	}
	return sshIncludeDisabled
}

func readSSHIncludePreference(dir string) (sshIncludePreference, error) {
	data, err := os.ReadFile(filepath.Join(dir, "include-preference"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	pref := sshIncludePreference(data)
	switch pref {
	case sshIncludeEnabled, sshIncludeDisabled:
		return pref, nil
	default:
		return "", errors.New("invalid SSH Include preference")
	}
}

// The managed SSH lock serializes saving the preference with config updates.
func writeSSHIncludePreference(dir string, pref sshIncludePreference) error {
	tmp, err := os.CreateTemp(dir, ".include-preference-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.WriteString(string(pref))
	if err := errors.Join(writeErr, tmp.Close()); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, "include-preference"))
}

func lockManagedSSH() (dir string, release func(), err error) {
	dir = filepath.Join(cliStateDir(), "ssh")
	if err := ensureStateDir(dir); err != nil {
		return "", nil, err
	}
	lock, err := acquireFileLock(filepath.Join(dir, sshConfigLockName), sshConfigLockWait)
	if err != nil {
		return "", nil, fmt.Errorf("lock Discobox SSH config: %w", err)
	}
	return dir, func() { _ = lock.Release() }, nil
}

func (a *App) newEnableSSHIncludeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "enable",
		Short: "Enable SSH Includes and refresh them from the configured servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := setSSHIncludePreference(cmd.Context(), sshIncludeEnabled); err != nil {
				return err
			}
			err := a.writeEverySSHConfig(cmd.Context(), "", nil, printedNotes(cmd.ErrOrStderr()))
			if err != nil {
				return fmt.Errorf("SSH Includes enabled for future syncs, but this sync failed: %w", err)
			}
			return nil
		},
	}
}

func newDisableSSHIncludeCommand() *cobra.Command {
	return &cobra.Command{
		Use: "disable", Short: "Remove Discobox Includes and keep them disabled",
		Args: cobra.NoArgs,
		// Disabling is local and works without resolving a server.
		PersistentPreRun: func(*cobra.Command, []string) {},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setSSHIncludePreference(cmd.Context(), sshIncludeDisabled)
		},
	}
}

func setSSHIncludePreference(ctx context.Context, pref sshIncludePreference) error {
	dir, release, err := lockManagedSSH()
	if err != nil {
		return err
	}
	defer release()
	if err := writeSSHIncludePreference(dir, pref); err != nil || pref == sshIncludeEnabled {
		return err
	}
	targets, err := machineSSHTargets(ctx)
	if err != nil {
		return err
	}
	errs := []error{targets.windowsErr}
	for _, target := range targets.all {
		errs = append(errs, target.removeOwnedIncludes(ctx))
	}
	return errors.Join(errs...)
}

func (t sshTarget) removeOwnedIncludes(ctx context.Context) error {
	edit, err := t.findSSHIncludes(t.ownsInclude)
	if err != nil || len(edit.removed) == 0 {
		return err
	}
	_, err = removeSSHIncludes(ctx, edit)
	return err
}

// Windows shares its state directory with the distributions that mirror SSH
// files into it. Only act on files emitted for this installation.
func (t sshTarget) ownsManagedConfig(config string) bool {
	if !strings.HasPrefix(config, managedConfigPrefix) {
		return false
	}
	if !t.windows {
		return true
	}
	for _, line := range strings.Split(config, "\n") {
		if distro, ok := strings.CutPrefix(line, "# WSL distribution: "); ok {
			return t.acrossWSL() && distro == t.wslDistro
		}
		if route, ok := strings.CutPrefix(line, "    ProxyCommand "); ok {
			if t.acrossWSL() {
				return strings.HasPrefix(route, "wsl.exe -d "+t.wslDistro+" -e sh -c ")
			}
			return !strings.HasPrefix(route, "wsl.exe -d ")
		}
	}
	return !t.acrossWSL()
}

func (t sshTarget) ownsInclude(path string) bool {
	if !t.within(t.sshDir().client, path) {
		return false
	}
	if !t.windows {
		return true
	}
	base := filepath.Base(filepath.Dir(filepath.FromSlash(t.clean(path))))
	// The distribution suffix establishes ownership even if a failed write
	// took back its generated config. Older files require inspecting the route.
	if _, hash, ok := strings.Cut(base, wslArtifactMarker); ok && len(hash) == 16 {
		if _, err := hex.DecodeString(hash); err == nil {
			return t.acrossWSL() && strings.HasSuffix(base, t.wslArtifactSuffix())
		}
	}
	data, err := os.ReadFile(t.configPath(base).local)
	if err != nil {
		return !t.acrossWSL() && os.IsNotExist(err)
	}
	return t.ownsManagedConfig(string(data))
}
