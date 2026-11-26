package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/cli/internal/winlsp"
	"github.com/discobox-ai/discobox/pkg/execstream/client"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// `discobox admin server lsp-exempt` exempts the local server from Winsock
// layered service providers on Windows (ADR 26-11-24-372). A VPN or proxy tool
// that installs an LSP has it loaded into every process using Winsock, and the
// server unmarshals the sockets WSL hands it for a guest process's stdio; an LSP
// that cannot take them fails every guest operation. Windows leaves such an LSP
// out of a process whose executable is marked LSP_SYSTEM, which is what WSL
// does for its own, and setting that mark needs administrator rights.
//
// The command is registered on Windows builds only (server_lsp_windows.go). It
// is written here, against lspCalls, so its decisions can be tested anywhere.

// lspExemptElevatedUse is the hidden subcommand the command starts elevated.
const lspExemptElevatedUse = "lsp-exempt-elevated"

// lspCalls are the Windows calls lsp-exempt makes: winlsp's on Windows, and a
// test's fakes elsewhere.
type lspCalls struct {
	category    func(path string) (uint32, error)
	setCategory func(path string, category uint32) error
	elevated    func() bool
	// runElevated starts the CLI elevated with args and returns its exit
	// code. winlsp.ErrDeclined is a prompt answered no.
	runElevated func(args []string) (uint32, error)
}

func (a *App) newServerLSPExemptCommand(calls lspCalls) *cobra.Command {
	var check, remove bool
	cmd := &cobra.Command{
		Use:   "lsp-exempt",
		Short: "Keep VPN and proxy Winsock providers out of the local server (asks for administrator rights)",
		Long: `Exempt the local server from Winsock layered service providers.

Some VPN and proxy tools install a layered service provider (an LSP), which
Windows loads into every program that uses the network. The local server takes
the connections WSL gives it for each program it runs in a pool's VM, and an LSP
that cannot handle them makes every one of those programs fail. Windows leaves
such an LSP out of a program marked as a system program, which is what WSL does
for its own programs. This command marks the server that way.

Marking it needs administrator rights, so the command asks for them with the
Windows administrator prompt, once. A server Discobox downloaded runs from the
same path whatever its version, so the mark outlives upgrades. A server named
with --binary or ` + ServerBinaryEnv + `, or one installed beside this command,
runs from where it is, and the mark applies to that path alone.

What it costs: the server's own network traffic (downloads, image pulls,
remote providers) no longer passes through LSPs that do not declare themselves
safe for system programs. VPNs that work at the network level are unaffected.

A running local server keeps the providers it started with. Stop it with
"discobox admin server shutdown" afterwards; the next command starts it again.

--check says whether the server is exempt and changes nothing. --remove takes
the mark off again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if check && remove {
				return errors.New("--check and --remove cannot be used together")
			}
			return a.lspExempt(cmd.Context(), cmd.OutOrStdout(), calls, check, remove)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "Only say whether the server is exempt; change nothing")
	cmd.Flags().BoolVar(&remove, "remove", false, "Take the exemption off the server")
	return cmd
}

// newServerLSPExemptElevatedCommand is what lsp-exempt runs elevated. It
// takes the path from its unelevated parent rather than resolving one: run
// with another administrator's credentials, it is another user, whose state
// directory is somewhere else.
//
// Its window is hidden, so what went wrong reaches the parent as its exit
// status alone (lspExemptExit). It writes nothing to disk but the category:
// any file the unelevated parent could name, a program running as that user
// could swap for a link to one only an administrator may write.
func newServerLSPExemptElevatedCommand(calls lspCalls) *cobra.Command {
	return &cobra.Command{
		Use:    lspExemptElevatedUse + " set|remove PATH",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			var exempt bool
			switch args[0] {
			case "set":
				exempt = true
			case "remove":
			default:
				return client.ExitError{Code: lspExemptExitUsage}
			}
			if err := validateLSPExemptPath(args[1]); err != nil {
				return client.ExitError{Code: lspExemptExitRefused}
			}
			if err := applyLSPExemption(calls, args[1], exempt); err != nil {
				return client.ExitError{Code: lspExemptExitWindows}
			}
			return nil
		},
	}
}

// The elevated step's exit statuses, which are all it can tell its parent.
const (
	lspExemptExitUsage   = 2
	lspExemptExitRefused = 3
	lspExemptExitWindows = 4
)

// lspExemptExit says what an elevated step's exit status means.
func lspExemptExit(code uint32, path string) error {
	switch code {
	case 0:
		return nil
	case lspExemptExitUsage:
		return errors.New("the elevated step was not given what it needs; is the discobox it ran the same version as this one?")
	case lspExemptExitRefused:
		return fmt.Errorf("the elevated step refused %s: it marks only an existing discobox-server.exe", path)
	case lspExemptExitWindows:
		return errors.New("the elevated step could not record the category: WSCSetApplicationCategory failed")
	default:
		return fmt.Errorf("the elevated step exited with status %d", code)
	}
}

// lspExemptTarget is the server lsp-exempt marks: the path the CLI would run
// it from. manifest is set when that is the fixed path every staged version
// runs from, and nil for a binary somebody chose, which runs where it is.
func (r serverResolver) lspExemptTarget(ctx context.Context) (path string, manifest *serverstage.Manifest, err error) {
	binary, manifest, err := r.choose(ctx)
	if err != nil {
		return "", nil, err
	}
	if manifest == nil {
		return binary, nil, nil
	}
	return manifest.RunCommand(r.stageRoot), manifest, nil
}

func (a *App) lspExempt(ctx context.Context, out io.Writer, calls lspCalls, check, remove bool) error {
	resolver := a.serverResolver(nil)
	path, manifest, err := resolver.lspExemptTarget(ctx)
	if err != nil {
		return err
	}
	pathAlone := func() {
		if manifest == nil {
			fmt.Fprintf(out, "This server was named with --binary or %s, or is installed beside discobox, so it runs from where it is: the exemption applies to that path alone.\n", ServerBinaryEnv)
		}
	}
	category, readErr := calls.category(path)
	exempt := readErr == nil && category&winlsp.LSPSystem != 0
	if check {
		if readErr != nil {
			return fmt.Errorf("read the Winsock exemption of %s: %w", path, readErr)
		}
		if exempt {
			fmt.Fprintf(out, "%s is exempt from Winsock layered service providers.\n", path)
		} else {
			fmt.Fprintf(out, "%s is not exempt from Winsock layered service providers. Run \"discobox admin server lsp-exempt\" to exempt it.\n", path)
		}
		pathAlone()
		return nil
	}
	switch {
	case remove && readErr == nil && !exempt:
		fmt.Fprintf(out, "%s is not exempt; there is nothing to remove.\n", path)
		return nil
	case !remove && exempt:
		fmt.Fprintf(out, "%s is already exempt from Winsock layered service providers.\n", path)
		pathAlone()
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if remove {
			return fmt.Errorf("%s is not there to take the exemption off; the entry it left in the Windows registry is harmless", path)
		}
		// The fixed path is filled only when a server is started from it.
		// Marking it now, before the first start, is staging and linking it
		// here.
		progress := a.serverStartupLine()
		resolver.onProgress = func(report serverstage.Progress) { progress.set(serverStageText(report)) }
		server, err := resolver.resolve(ctx)
		if err == nil {
			path, err = resolver.runPath(server)
		}
		progress.clear()
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	// Checked here as the elevated step checks it, so a path it would refuse
	// is said here, before the prompt, rather than lost in a hidden window
	// after it.
	if err := validateLSPExemptPath(path); err != nil {
		return err
	}
	if !remove {
		fmt.Fprintf(out, "Exempting %s from Winsock layered service providers.\n"+
			"Its own network traffic will no longer pass through VPN or proxy providers that do not declare themselves safe for system programs.\n", path)
	}
	verb := "set"
	if remove {
		verb = "remove"
	}
	var stepErr error
	if calls.elevated() {
		stepErr = applyLSPExemption(calls, path, !remove)
	} else {
		fmt.Fprintln(out, "Windows will ask for administrator rights.")
		code, err := calls.runElevated([]string{"admin", "server", lspExemptElevatedUse, verb, path})
		switch {
		case errors.Is(err, winlsp.ErrDeclined):
			return fmt.Errorf("%w; nothing was changed", err)
		case err != nil:
			return err
		}
		stepErr = lspExemptExit(code, path)
	}
	// What is reported is what Windows now says, never what was meant to
	// happen.
	category, err = calls.category(path)
	if err != nil {
		return errors.Join(stepErr, fmt.Errorf("read the Winsock exemption of %s back: %w", path, err))
	}
	switch now := category&winlsp.LSPSystem != 0; {
	case remove && now:
		return errors.Join(stepErr, fmt.Errorf("%s is still exempt", path))
	case !remove && !now:
		return errors.Join(stepErr, fmt.Errorf("%s is still not exempt", path))
	}
	if remove {
		fmt.Fprintf(out, "%s is no longer exempt.\n", path)
	} else {
		fmt.Fprintf(out, "%s is exempt.\n", path)
		pathAlone()
	}
	fmt.Fprintln(out, "Windows reads this when a program starts, so a local server that is running keeps what it started with. Stop it with \"discobox admin server shutdown\"; the next command starts it again.")
	return nil
}

// applyLSPExemption sets or clears LSPSystem on path, keeping every other bit
// of its category. It runs elevated, from a path its unelevated parent passed
// on, so it marks nothing but an existing discobox-server.exe.
func applyLSPExemption(calls lspCalls, path string, exempt bool) error {
	if err := validateLSPExemptPath(path); err != nil {
		return err
	}
	// A category that cannot be read is taken as empty. What is not known
	// cannot be kept, and the parent reads the result back either way.
	category, err := calls.category(path)
	if err != nil {
		category = 0
	}
	if exempt {
		category |= winlsp.LSPSystem
	} else {
		category &^= winlsp.LSPSystem
	}
	return calls.setCategory(path, category)
}

// validateLSPExemptPath refuses anything but an existing file named
// discobox-server.exe, given as an absolute path.
func validateLSPExemptPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s is not an absolute path", path)
	}
	if !strings.EqualFold(filepath.Base(path), "discobox-server.exe") {
		return fmt.Errorf("%s is not a discobox-server.exe", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a file", path)
	}
	return nil
}

// lspExemptionKept is the Winsock exemption of each file in the fixed run
// directory that has one, as something an uninstall leaves behind. The
// directory is read rather than a path built for it: what is there is what was
// exempted. The entry is in HKLM, and an uninstall does not elevate; it says
// the entry stays, that it is harmless, and how to take it off while the file
// is still there to name.
func lspExemptionKept(runDir string, category func(string) (uint32, error)) []uninstallKept {
	entries, err := os.ReadDir(runDir)
	if err != nil {
		return nil
	}
	var kept []uninstallKept
	for _, entry := range entries {
		path := filepath.Join(runDir, entry.Name())
		if !entry.Type().IsRegular() {
			continue
		}
		if current, err := category(path); err != nil || current&winlsp.LSPSystem == 0 {
			continue
		}
		kept = append(kept, uninstallKept{
			path:   path,
			what:   "Winsock exemption in the Windows registry",
			reason: "removing it needs administrator rights, and it is harmless once the server is gone; to remove it, answer N and run \"discobox admin server lsp-exempt --remove\" first",
		})
	}
	return kept
}
