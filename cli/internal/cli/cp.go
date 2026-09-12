package cli

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/endpoint"
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
ID, a short prefix of it, or the name the discobox was created with. That name
is the NAME "discobox ls" prints only until the discobox's agent titles its
terminal and the title takes the column, so the ID is what always resolves; the
title itself is a name "discobox rm" takes, not this one. A bare :PATH means the
discobox this directory started, or a prompt to pick one when there is more than
one. A discobox on another server is written by its address,
DISCOBOX_ADDRESS:PATH — discobox://<server>/<discobox> as "discobox ls" and
"discobox servers" give it. Everything without a colon is a local path.

Both ends may name a discobox, and they need not be the same discobox — but
they must be on the same server, since one copy runs over one connection. An
address says which server that is, and a name or a bare :PATH beside one is
looked up there rather than on the primary.

Relative remote paths are resolved from the discobox user's home directory, not
from a source working tree.

The server needs no SSH port for this: the transfer is carried over the same
endpoint the API uses, through a loopback port that exists only while the
command runs. Key and host verification are supplied here, so nothing is written
to your ssh_config; the key is enrolled in the project and reused on later runs.

Every other argument is passed to scp untouched, so its own flags — -r, -p, -C,
-o — mean what they always mean. That includes the ones this CLI otherwise
takes: -p is scp's preserve here, not --project. To point a copy somewhere
else, write the flag in front of the command — discobox --server ... cp — or
set DISCOBOX_SERVER and DISCOBOX_PROJECT in the environment.`,
		Example: `  discobox cp ./config.yaml mybox:/tmp/config.yaml
  discobox cp -r mybox:/workspace/dist ./dist
  discobox cp :notes.md .
  discobox cp mybox:/tmp/a.txt otherbox:/tmp/a.txt
  discobox cp discobox://box.example.com/sbx_01hq:/workspace/out.txt .`,
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
	target, err := a.resolveCPTarget(cmd, operands)
	if err != nil {
		return err
	}
	rewritten, err := target.app.resolveCPOperands(cmd, target.client, target.projectID, operands, target.resolved)
	if err != nil {
		return err
	}

	bridge, err := target.app.startSSHBridgeSession(cmd, target.client, target.projectID)
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

// cpTarget is the server a copy runs against and what it took to decide: the
// App aimed at it, its client and project, and the discoboxes already resolved
// on the way, since an address resolves as it is read.
//
// Every reference in one command resolves there, a name and a bare `:PATH`
// included: one scp runs over one bridge, so the server an address names is
// the only one this copy can reach.
type cpTarget struct {
	app       *App
	client    *apiclientgen.Client
	projectID string
	// resolved is reference -> discobox ID for the addresses read here, so
	// resolveCPOperands does not resolve them a second time.
	resolved map[string]string
}

// resolveCPTarget decides which server the copy runs against.
//
// An address names its server (ADR 0114 §6), so the first one decides it —
// and registers it, since it resolves through selectSandbox like every other
// address. One scp runs over one bridge, so a second address naming a
// different server is refused rather than half-copied; a name or an ID in the
// same command then resolves on the server the address chose, which is the
// only one this copy can reach.
func (a *App) resolveCPTarget(cmd *cobra.Command, operands []cpOperand) (cpTarget, error) {
	target := cpTarget{app: a, resolved: map[string]string{}}
	// Everything decidable from the operands is decided first, so a copy this
	// cannot make contacts nothing — and registers nothing, since reaching a
	// server through its address registers it (ADR 0114 §6) and a refused copy
	// must not leave one behind.
	addresses := make([]string, 0, len(operands))
	servers := map[string]string{}
	for _, operand := range operands {
		if !operand.remote || !discoboxAddressOperand(operand.reference) {
			continue
		}
		switch {
		case operand.addressWithoutPath:
			// An address naming no discobox at all — discobox://host — is that
			// before it is anything about paths, and ParseSandboxAddress has
			// the words for it; advising a path here would send the reader
			// round again.
			if _, _, err := endpoint.ParseSandboxAddress(operand.reference); err != nil {
				return cpTarget{}, err
			}
			return cpTarget{}, fmt.Errorf("%s names a discobox but no file: an operand is <discobox>:<path>, so write %s:/path, or %s: for its home directory",
				operand.reference, operand.reference, operand.reference)
		case operand.addressWithQuery:
			return cpTarget{}, fmt.Errorf("%s carries a query, and cp cannot tell where it ends: ?addr= holds host:port, so the colon after it is as likely the port's as the path's. "+
				"Register the server once (`discobox servers add <address>`) and name the discobox on it instead", operand.reference)
		}
		address, _, err := endpoint.ParseSandboxAddress(operand.reference)
		if err != nil {
			return cpTarget{}, err
		}
		if _, seen := servers[serverKey(address.Server)]; !seen {
			servers[serverKey(address.Server)] = address.Server
		}
		if len(servers) > 1 {
			named := slices.Sorted(maps.Values(servers))
			return cpTarget{}, fmt.Errorf("one copy reaches one server, and these name two (%s and %s): copy through this machine in two commands", named[0], named[1])
		}
		if !slices.Contains(addresses, operand.reference) {
			addresses = append(addresses, operand.reference)
		}
	}
	for _, reference := range addresses {
		app, projectID, sandboxID, client, err := a.selectSandbox(cmd, reference)
		if err != nil {
			return cpTarget{}, err
		}
		target.app, target.client, target.projectID = app, client, projectID
		target.resolved[reference] = sandboxID
	}
	if target.client != nil {
		return target, nil
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return cpTarget{}, err
	}
	client, err := a.apiClient()
	if err != nil {
		return cpTarget{}, err
	}
	target.projectID, target.client = projectID, client
	return target, nil
}

// cpOperand is one path to copy, as written: local, or inside the discobox a
// reference names.
type cpOperand struct {
	remote bool
	// reference is what stood before the colon — a name, an ID, a prefix, or
	// empty for the bare `:PATH` form.
	reference string
	// path is the file itself: the remote side of the colon, or the whole
	// operand spelled so scp reads it as local. Empty is the discobox's home
	// directory, which is what a trailing colon means — `mybox:` and
	// `discobox://host/sbx:` alike.
	path string
	// addressWithoutPath marks an address with no separator after it, which
	// names a discobox and no file; addressWithQuery one carrying a query,
	// which cannot be split from a path. resolveCPTarget refuses both by name,
	// before anything is contacted.
	addressWithoutPath bool
	addressWithQuery   bool
}

func parseCPOperands(paths []string) []cpOperand {
	operands := make([]cpOperand, 0, len(paths))
	for _, path := range paths {
		if discoboxAddressOperand(path) {
			reference, remotePath, hasPath, hasQuery := splitCPAddress(path)
			operands = append(operands, cpOperand{
				remote: true, reference: reference, path: remotePath,
				addressWithoutPath: !hasPath && !hasQuery, addressWithQuery: hasQuery,
			})
			continue
		}
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
func (a *App) resolveCPOperands(cmd *cobra.Command, client *apiclientgen.Client, projectID string, operands []cpOperand, resolved map[string]string) ([]string, error) {
	if resolved == nil {
		resolved = map[string]string{}
	}
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
// It is `shell`'s rule rather than `--discobox-id`'s, so it resolves through
// resolveSandboxReference: the reference is something the user typed alongside
// a path, so a name has to work there the way it works in
// `discobox shell mybox ls`. `selectSandbox` cannot serve — with a non-empty
// argument it resolves IDs only, and hands a name straight back, which here
// would become an SSH username no sandbox answers to.
//
// A name stays matched per directory even though the picker's "a" *shows*
// names from the whole project. Picking such a row is still fine — the picker
// hands back an ID, not the name — so what a widened pick costs is only that
// the name cannot be retyped later, which is what resolveSandboxReference's
// error says when someone tries.
func (a *App) resolveCPSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID, reference string) (string, error) {
	sandboxes, err := a.listProjectSandboxCandidates(cmd.Context(), client, projectID, false)
	if err != nil {
		return "", err
	}
	if reference == "" {
		return pickOne(cmd, "Select a discobox", sandboxPickerItems(sandboxes, ""), pickerOptions{
			empty:     "no discoboxes were started from this directory; start one with `discobox run`, or name one before the colon",
			ambiguous: "more than one discobox was started from this directory; name one before the colon",
			recentKey: "sandbox:" + projectID,
			expand:    a.sandboxPickerExpansion(cmd.Context(), client, projectID),
		})
	}
	// configuredName: what cp accepts before a colon is `shell`'s rule, and
	// widening it to the window title is a change to cp, not to this command.
	return a.resolveSandboxReference(cmd.Context(), client, projectID, reference, sandboxes, configuredName)
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
	// A discobox's address carries colons and slashes of its own, so it is
	// split at the colon that ends the discobox rather than at the first one:
	// discobox://host:8443/sbx_01:/tmp/x. The prefix is what makes this safe to
	// look for — the operand it could be mistaken for is a discobox named
	// "discobox" copied to a path starting "//". parseCPOperands reads the two
	// cases this cannot report here; see splitCPAddress.
	if discoboxAddressOperand(operand) {
		reference, path, _, _ := splitCPAddress(operand)
		return reference, path, true
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

// splitCPAddress splits an operand written as a discobox's address. The
// authority runs to the first slash after the scheme and may carry a colon of
// its own (a port); the discobox segment follows it and can carry neither, so
// the next colon is the one that ends the address. An address with no colon
// after it names no path, which resolveCPTarget reports.
// splitCPAddress splits an operand written as a discobox's address into the
// address and the path after it.
//
// The address ends where its discobox segment does: the authority runs to the
// first slash after the scheme and may carry a port, and a discobox segment
// carries neither ":" nor "?" nor "/", so whichever of ":" or "?" comes first
// after it is the end. A ":" there is the separator; a "?" is a query, which
// this cannot split past — ?addr= carries host:port, so the next colon is as
// likely the port's as the path's — and hasQuery says so for the caller to
// refuse by name.
//
// hasPath distinguishes "no separator at all" from a trailing colon, which is
// the home directory exactly as `mybox:` is.
func splitCPAddress(operand string) (reference, path string, hasPath, hasQuery bool) {
	scheme := strings.Index(operand, "://")
	authority := operand[scheme+len("://"):]
	slash := strings.Index(authority, "/")
	if slash < 0 {
		return operand, "", false, false
	}
	end := scheme + len("://") + slash
	for i, r := range operand[end:] {
		switch r {
		case ':':
			return operand[:end+i], operand[end+i+1:], true, false
		case '?':
			return operand, "", false, true
		}
	}
	return operand, "", false, false
}

// discoboxAddressOperand reports whether an operand is written as a discobox
// address rather than as <discobox>:<path>.
func discoboxAddressOperand(operand string) bool {
	lowered := strings.ToLower(strings.TrimSpace(operand))
	for _, scheme := range []string{endpoint.SchemeDiscobox, endpoint.SchemeDiscoboxHTTP, endpoint.SchemeDiscoboxHTTPS} {
		if strings.HasPrefix(lowered, scheme+"://") {
			return true
		}
	}
	return false
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
