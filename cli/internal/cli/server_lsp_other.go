//go:build !windows

package cli

import "github.com/spf13/cobra"

// addPlatformServerCommands adds nothing: lsp-exempt is Windows's alone, and
// nothing on another platform is exempted by path.
func (a *App) addPlatformServerCommands(*cobra.Command) {}

// platformUninstallKept is nothing: only Windows keeps an exemption by path.
func platformUninstallKept() []uninstallKept { return nil }
