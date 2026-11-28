//go:build windows

package cli

import (
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/winlsp"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// windowsLSPCalls are the real Winsock and UAC calls lsp-exempt makes.
var windowsLSPCalls = lspCalls{
	category:    winlsp.Category,
	setCategory: winlsp.SetCategory,
	elevated:    winlsp.Elevated,
	runElevated: func(args []string) (uint32, error) {
		self, err := os.Executable()
		if err != nil {
			return 0, err
		}
		return winlsp.RunElevated(self, args)
	},
}

// addPlatformServerCommands adds lsp-exempt, which only Windows has.
func (a *App) addPlatformServerCommands(cmd *cobra.Command) {
	cmd.AddCommand(a.newServerLSPExemptCommand(windowsLSPCalls))
	cmd.AddCommand(newServerLSPExemptElevatedCommand(windowsLSPCalls))
}

// platformUninstallKept is the Winsock exemption of whatever runs from this
// platform's fixed run directory, which an uninstall leaves in the registry.
func platformUninstallKept() []uninstallKept {
	return lspExemptionKept(serverstage.RunDir(stagedServerRoot(), runtime.GOOS, runtime.GOARCH), winlsp.Category)
}
