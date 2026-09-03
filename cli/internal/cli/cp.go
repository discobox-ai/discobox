package cli

import (
	"fmt"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// newCPCommand implements `discobox cp`: scp(1), pointed at the same SSH
// ingress `discobox tools ssh` uses.
//
// Nothing here copies bytes. The server's sshd already answers the `sftp`
// subsystem by running the sandbox's `sftp-server` as an exec
// (`server/internal/sshd/session.go`), which is exactly what a modern scp
// speaks, so the whole transfer — recursion, permissions, resumed directories —
// is scp's and the sandbox's business. What this command owns is the one thing
// scp cannot work out for itself: which loopback port, key and host key reach a
// discobox, and which discobox a `NAME:PATH` argument means.
func (a *App) newCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cp [SCP_ARG...] SRC... DST",
		Aliases: []string{"scp"},
		Short:   "Copy files to and from a discobox",
		Long: `Copy files between this machine and a discobox, with scp.

A path is inside a discobox when it is written DISCOBOX:PATH — the discobox's
name, its ID, or a short prefix of either, exactly as "discobox ls" shows them.
A bare :PATH means the discobox this directory started, or a prompt to pick one
when there is more than one. Everything without a colon is a local path.

Both ends may name a discobox, and they need not be the same one.

Relative remote paths are resolved from the discobox user's home directory, not
from a source working tree.

The server needs no SSH port for this: the transfer is carried over the same
endpoint the API uses, through a loopback port that exists only while the
command runs. Key and host verification are supplied here, so nothing is written
to your ssh_config; the key is enrolled in the project and reused on later runs.

Every other argument is passed to scp untouched, so its own flags — -r, -p, -C,
-o — mean what they always mean. That includes the ones this CLI otherwise
takes: -p is scp's preserve here, not --project. Set DISCOBOX_SERVER and
DISCOBOX_PROJECT in the environment to point a copy somewhere else.`,
		Example: `  discobox cp ./config.yaml mybox:/tmp/config.yaml
  discobox cp -r mybox:/workspace/dist ./dist
  discobox cp :notes.md .
  discobox cp mybox:/tmp/a.txt otherbox:/tmp/a.txt`,
		// Flag parsing is off entirely, not just SetInterspersed(false): scp's
		// own flags come first in the common case (`discobox cp -r ...`), and
		// cobra would reject them as unknown before the command ever ran.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			return a.runCP(cmd, args)
		},
	}
	return cmd
}

func (a *App) runCP(cmd *cobra.Command, args []string) error {
	options, paths := splitSCPArgs(args)
	if len(paths) < 2 {
		return fmt.Errorf("cp needs at least one source and a destination; see `discobox cp --help`")
	}
	// The operands are read before anything is contacted, so a command that
	// asks for a copy this cannot make — one with no discobox in it — says so
	// without first starting a server or opening a bridge.
	operands := parseCPOperands(paths)
	if !slices.ContainsFunc(operands, func(operand cpOperand) bool { return operand.remote }) {
		return fmt.Errorf("no discobox was named: write a path as DISCOBOX:PATH, or :PATH for this directory's discobox")
	}

	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	rewritten, err := a.resolveCPOperands(cmd, client, projectID, operands)
	if err != nil {
		return err
	}

	bridge, err := a.startSSHBridgeSession(cmd, client, projectID)
	if err != nil {
		return err
	}
	defer bridge.close()

	return runOverSSHBridge(cmd, "scp", scpArgs(scpInvocation{
		bridge:   scpBridgeArgs(bridge.port(), bridge.identity, bridge.knownHosts),
		options:  options,
		operands: rewritten,
		remote:   cpOperandsAreRemote(operands),
	}))
}

// cpOperand is one path to copy, as written: local, or inside the discobox a
// reference names.
type cpOperand struct {
	remote bool
	// reference is what stood before the colon — a name, an ID, a prefix, or
	// empty for the bare `:PATH` form.
	reference string
	// path is the file itself: the remote side of the colon, or the whole
	// operand spelled so scp reads it as local.
	path string
}

func parseCPOperands(paths []string) []cpOperand {
	operands := make([]cpOperand, 0, len(paths))
	for _, path := range paths {
		reference, remotePath, remote := splitCPPath(path)
		if !remote {
			operands = append(operands, cpOperand{path: localSCPPath(path)})
			continue
		}
		operands = append(operands, cpOperand{remote: true, reference: reference, path: remotePath})
	}
	return operands
}

func cpOperandsAreRemote(operands []cpOperand) []bool {
	remote := make([]bool, len(operands))
	for i, operand := range operands {
		remote[i] = operand.remote
	}
	return remote
}

// scpInvocation is everything the argument list is assembled from: what points
// scp at the bridge, the user's own options, the rewritten operands, and which
// of those operands are inside a discobox.
type scpInvocation struct {
	bridge   []string
	options  []string
	operands []string
	remote   []bool
}

// scpArgs assembles the argument list in the order `scp [options] source ...
// target` requires.
func scpArgs(invocation scpInvocation) []string {
	// Cloned, not appended to in place: scpBridgeArgs leaves spare capacity
	// behind, and appending into a caller's slice would write past what it
	// thinks it owns.
	args := slices.Clone(invocation.bridge)
	last := len(invocation.remote) - 1
	if last >= 0 && invocation.remote[last] && slices.Contains(invocation.remote[:last], true) {
		// Discobox to discobox, routed through this process — the only place
		// both ends are reachable from, since each is a loopback port on this
		// machine that means nothing inside a sandbox. Current OpenSSH already
		// routes an sftp-mode copy this way and -3 changes nothing there; it is
		// pinned because the direct path is one `-R`, one older client, or one
		// ssh_config default away, and it cannot work here — the source dials
		// 127.0.0.1:22 inside its own sandbox and is refused.
		args = append(args, "-3")
	}
	args = append(args, invocation.options...)
	// `--` ends scp's options for good: a rewritten remote operand is
	// `sbx_…@127.0.0.1:…` and can never look like a flag, but a local one the
	// user wrote as `-x` still would, and scp reads options after operands the
	// way glibc's getopt permutes them.
	args = append(args, "--")
	return append(args, invocation.operands...)
}

// resolveCPOperands turns each remote operand into the `USER@HOST:PATH` scp
// takes, leaving local ones alone.
//
// Each distinct reference is resolved once. Repeating the resolution would cost
// a round trip per operand, and — for the bare `:PATH` form, which has no
// reference to resolve — would open the picker again for every argument that
// used it.
func (a *App) resolveCPOperands(cmd *cobra.Command, client *apiclientgen.Client, projectID string, operands []cpOperand) ([]string, error) {
	resolved := map[string]string{}
	rewritten := make([]string, 0, len(operands))
	for _, operand := range operands {
		if !operand.remote {
			rewritten = append(rewritten, operand.path)
			continue
		}
		sandboxID, seen := resolved[operand.reference]
		if !seen {
			var err error
			if sandboxID, err = a.resolveCPSandbox(cmd, client, projectID, operand.reference); err != nil {
				return nil, err
			}
			resolved[operand.reference] = sandboxID
		}
		rewritten = append(rewritten, sandboxID+"@"+sshBridgeHost+":"+operand.path)
	}
	return rewritten, nil
}

// resolveCPSandbox turns what stood before the colon into a sandbox ID.
//
// It is `shell`'s rule rather than `--discobox-id`'s: the reference is
// something the user typed alongside a path, so a name has to work there the
// way it works in `discobox shell mybox ls`. `selectSandbox` cannot serve —
// with a non-empty argument it resolves IDs only, and hands a name straight
// back, which here would become an SSH username no sandbox answers to.
//
// An ID that names no discobox of this directory is still tried against the
// whole project: a discobox started somewhere else is still a discobox, and an
// ID says outright which one. A name is not, because names are only ever shown
// — and matched — per directory.
func (a *App) resolveCPSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID, reference string) (string, error) {
	sandboxes, err := a.listProjectSandboxCandidates(cmd.Context(), client, projectID)
	if err != nil {
		return "", err
	}
	if reference == "" {
		return pickOne(cmd, "Select a discobox", sandboxPickerItems(sandboxes), pickerOptions{
			empty:     "no discoboxes were started from this directory; start one with `discobox run`, or name one before the colon",
			ambiguous: "more than one discobox was started from this directory; name one before the colon",
			recentKey: "sandbox:" + projectID,
		})
	}
	sandboxID, ok, err := matchSandboxArg(reference, sandboxes)
	if err != nil {
		return "", err
	}
	if ok {
		return sandboxID, nil
	}
	if !isResolvableShortID(reference) {
		return "", fmt.Errorf("no discobox named %q was started from this directory; run `discobox ls` to see them, or write the discobox ID", reference)
	}
	sandboxID, err = a.resolveSandboxID(cmd.Context(), client, projectID, reference)
	if err != nil {
		// The reference is shaped like a short ID, so it was tried as one —
		// but a name is what someone writing `mybox:/tmp/x` most likely meant,
		// and matchSandboxArg already ruled that out. Reporting only the ID
		// reading sends them looking for an ID problem they do not have.
		return "", fmt.Errorf("no discobox for %q: it names none started from this directory, and %w", reference, err)
	}
	return sandboxID, nil
}

// splitCPPath decides whether an operand names a discobox, and splits it if it
// does.
//
// The rule is scp's own (`colon()` in scp.c), with one deliberate difference: a
// leading colon is a discobox reference here rather than part of a filename.
// scp has no use for the form — `:x` is just the file `./x` — and it is the
// natural spelling for "the discobox I am already working in", which is the
// most common thing to mean.
func splitCPPath(operand string) (reference, path string, remote bool) {
	if windowsDrivePath(operand) {
		return "", "", false
	}
	for i, r := range operand {
		switch r {
		case ':':
			return operand[:i], operand[i+1:], true
		case '/':
			// A slash first means every colon after it is inside a filename,
			// which is why `./weird:name` is local and `weird:name` is not.
			return "", "", false
		}
	}
	return "", "", false
}

// localSCPPath spells a local operand so scp reads it as one. scp applies the
// same colon rule this command does, so an operand that only survived
// splitCPPath because a `/` came first — `sub/dir:name` — would be split by scp
// after all. A leading `./` puts the slash first for scp too.
func localSCPPath(operand string) string {
	if !strings.Contains(operand, ":") || strings.HasPrefix(operand, "/") || strings.HasPrefix(operand, "./") || windowsDrivePath(operand) {
		return operand
	}
	return "./" + operand
}

// windowsDrivePath reports whether an operand is a Windows path whose colon
// belongs to a drive letter. Only on Windows: `C:/src` names a directory there
// and a host called `C` everywhere else, and OpenSSH's own Windows port draws
// the same line.
func windowsDrivePath(operand string) bool {
	if runtime.GOOS != "windows" || len(operand) < 2 || operand[1] != ':' {
		return false
	}
	letter := operand[0]
	return (letter >= 'a' && letter <= 'z') || (letter >= 'A' && letter <= 'Z')
}

// scpOptionsWithValue are scp(1)'s options that consume a value, either
// attached (`-l1024`) or as the next argument (`-l 1024`). Everything else scp
// accepts is a boolean flag.
//
// The table exists because the operands have to be found: this command rewrites
// them, and `-o ProxyJump=x` must not be mistaken for a path.
var scpOptionsWithValue = map[rune]bool{
	'c': true, 'D': true, 'F': true, 'i': true, 'J': true,
	'l': true, 'o': true, 'P': true, 'S': true, 'X': true,
}

// splitSCPArgs divides the user's arguments into scp's options and the paths to
// copy.
//
// Options are collected wherever they appear and emitted before the operands,
// which is where scp's usage puts them. Placing them after happens to work on
// glibc, whose getopt permutes argv, and is read as a filename anywhere else.
//
// `--` ends the options and is not forwarded: this command appends its own
// separator once the operands are rewritten.
func splitSCPArgs(args []string) (options, paths []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return options, append(paths, args[i+1:]...)
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			paths = append(paths, arg)
			continue
		}
		options = append(options, arg)

		// Walk the bundle: -rp is two booleans, -rl 1024 ends in an option
		// whose value is the next argument, and -rl1024 carries it inline.
		// Anything after a value-taking letter belongs to that value, which is
		// why the scan stops there — the `r` in `-o r=1` is not a flag.
		runes := []rune(arg[1:])
		for index, letter := range runes {
			if scpOptionsWithValue[letter] {
				if index == len(runes)-1 && i+1 < len(args) {
					i++
					options = append(options, args[i])
				}
				break
			}
		}
	}
	return options, paths
}
