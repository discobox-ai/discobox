package cli

import (
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	idpkg "github.com/discobox-ai/x/id"
)

func (a *App) newCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "cp [-r] [-p] [-q] [--] SRC... DST",
		Aliases: []string{"scp"},
		Short:   "Copy files to and from a discobox",
		Long: `Copy files through the discobox files API, as the discobox's default user.

Write a remote path as DISCOBOX:PATH, :PATH to select a discobox, or
DISCOBOX_ADDRESS:PATH. Relative remote paths start at the user's home.
Both ends may name discoboxes on the same server. Local paths follow the
usual colon and Windows drive rules; use ./ before an ambiguous local name.

Use -r for directories, -p to preserve ordinary permissions and access and
modification times, and -q for quiet operation. Multiple sources need an
existing destination directory. Existing directories receive each source's
basename; a missing destination takes its content. A trailing slash requires
a directory. Unrelated destination entries remain in place.

Remote paths are literal: spaces, Unicode, colons and shell characters are
filenames, without shell or glob expansion. -- ends options. Source links are
followed; links inside a recursive tree must stay within that tree. Destination
links and special files are refused. A failed recursive copy may leave completed
files, but never publishes a truncated current file.

Copy no longer uses SSH or scp. SSH/transport flags and other unsupported scp
options are errors; supported flags are -r, -p and -q. Set global options before
cp, such as discobox --project PROJECT cp ...`,
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
	options, paths, err := parseCopyArgs(args)
	if err != nil {
		return err
	}
	if len(paths) < 2 {
		return fmt.Errorf("cp needs at least one source and a destination; see `discobox cp --help`")
	}
	// The operands are read before anything is contacted, so a command that
	// asks for a copy this cannot make — one with no discobox in it — says so
	// without first starting a server or enrolling a key.
	operands := parseCPOperands(paths)
	if !slices.ContainsFunc(operands, func(operand cpOperand) bool { return operand.remote }) {
		return fmt.Errorf("no discobox was named: write a path as DISCOBOX:PATH, or :PATH for the only discobox, or to pick one")
	}
	target, err := a.resolveCPTarget(cmd, operands)
	if err != nil {
		return err
	}
	operands, err = target.app.resolveCPOperands(cmd, target.client, target.projectID, operands, target.resolved)
	if err != nil {
		return err
	}
	return target.app.copyOperands(cmd.Context(), target.client, target.projectID, operands, options)
}

// cpTarget is the server a copy runs against and what it took to decide: the
// App aimed at it, its client and project, and the discoboxes already resolved
// on the way, since an address resolves as it is read.
//
// Every reference in one command resolves there, a name and a bare `:PATH`
// included: one scp runs one ProxyCommand, aimed at one server, so the server
// an address names is the only one this copy can reach.
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
// An address names its server (ADR 0116 §6), so the first one decides it —
// and registers it, since it resolves through selectSandbox like every other
// address. One scp is pointed at one server, so a second address naming a
// different server is refused rather than half-copied; a name or an ID in the
// same command then resolves on the server the address chose, which is the
// only one this copy can reach. With no address, every reference is looked for
// on every server (ADR 0116 §4) once there is more than one, and the server
// they are on is the one the copy runs against.
func (a *App) resolveCPTarget(cmd *cobra.Command, operands []cpOperand) (cpTarget, error) {
	target := cpTarget{app: a, resolved: map[string]string{}}
	// Everything decidable from the operands is decided first, so a copy this
	// cannot make contacts nothing — and registers nothing, since reaching a
	// server through its address registers it (ADR 0116 §6) and a refused copy
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
				"Register the server once (`discobox admin remote add <address>`) and name the discobox on it instead", operand.reference)
		}
		address, _, err := endpoint.ParseSandboxAddress(operand.reference)
		if err != nil {
			return cpTarget{}, err
		}
		if address.Local() {
			// On the primary, as selectAddressedSandbox reads it.
			address.Server = a.serverURL
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
	set, err := a.servers()
	if err != nil {
		return cpTarget{}, err
	}
	if len(set) > 1 {
		return a.resolveCPTargetOnEveryServer(cmd, set, operands)
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

// resolveCPTargetOnEveryServer is resolveCPTarget for a copy that names no
// address once more than one server is known (ADR 0116 §4). Each distinct
// reference is resolved by resolveCPSandbox's rule across every server, and
// the copy runs against the server they are on — which has to be one server,
// for the reason an address has to be.
func (a *App) resolveCPTargetOnEveryServer(cmd *cobra.Command, set []*server, operands []cpOperand) (cpTarget, error) {
	var (
		on          *server
		firstID     string
		candidates  []serverSandbox
		unreachable []unansweredServer
		listed      bool
	)
	resolved := map[string]string{}
	for _, operand := range operands {
		if !operand.remote {
			continue
		}
		if _, seen := resolved[operand.reference]; seen {
			continue
		}
		// Listed once, and only for a reference that needs it: a copy naming
		// its discoboxes by full ID asks for them and nothing else.
		if !listed && !idpkg.IsGenerated(idpkg.Canonical(idpkg.PrefixSandbox, operand.reference)) {
			var err error
			if candidates, unreachable, err = a.sandboxCandidates(cmd.Context()); err != nil {
				return cpTarget{}, err
			}
			listed = true
		}
		s, sandboxID, err := a.resolveCPSandboxOnEveryServer(cmd, set, candidates, unreachable, operand.reference)
		if err != nil {
			return cpTarget{}, err
		}
		if on != nil && s != on {
			return cpTarget{}, fmt.Errorf("one copy reaches one server, and these discoboxes are on two (%s on %s, %s on %s): copy through this machine in two commands",
				firstID, on.name, sandboxID, s.name)
		}
		if on == nil {
			on, firstID = s, sandboxID
		}
		resolved[operand.reference] = sandboxID
	}
	projectID, client, err := on.projectClient(cmd.Context())
	if err != nil {
		return cpTarget{}, err
	}
	return cpTarget{app: on.app, client: client, projectID: projectID, resolved: resolved}, nil
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

// resolveCPSandbox turns what stood before the colon into a sandbox ID.
//
// It resolves through resolveSandboxReference over the candidates it lists,
// under configuredName rather than selectSandbox's listed names: what stands
// before a colon may be a local path's, so it is a discobox only when it names
// one by the name it was created with or an ID, and a free-form window title
// is not matched against it.
func (a *App) resolveCPSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID, reference string) (string, error) {
	sandboxes, err := a.listProjectSandboxCandidates(cmd.Context(), client, projectID)
	if err != nil {
		return "", err
	}
	if reference == "" {
		localHostID, err := hostid.Get()
		if err != nil {
			return "", err
		}
		return pickOne(cmd, "Select a discobox", sandboxPickerItems(sandboxes, localHostID), pickerOptions{
			empty:     cpPickEmpty,
			ambiguous: cpPickAmbiguous,
			recentKey: "sandbox:" + projectID,
		})
	}
	// configuredName: widening what cp accepts before a colon to the window
	// title is a change to cp, decided on its own.
	return a.resolveSandboxReference(cmd.Context(), client, projectID, reference, sandboxes, configuredName)
}

// What cp's picker says when a bare :PATH has no discobox to mean, or more
// than one and nobody to ask: the way out of the second is a reference before
// the colon.
const (
	cpPickEmpty     = "there are no discoboxes; start one with `discobox run`"
	cpPickAmbiguous = "there is more than one discobox; name one before the colon"
)

// resolveCPSandboxOnEveryServer is resolveCPSandbox across every server, and
// the server the discobox is on. candidates is what `discobox ls` lists across
// them, which a full generated ID does not need: that is asked of every server
// directly, the primary first.
//
// The rest keeps resolveSandboxReference's asymmetry. A name or short ID is
// matched among the candidates; a short ID that matches none is still looked
// for project-wide, on every server; a name that matches none is an error.
func (a *App) resolveCPSandboxOnEveryServer(cmd *cobra.Command, set []*server, candidates []serverSandbox, unreachable []unansweredServer, reference string) (*server, string, error) {
	if reference == "" {
		return a.pickServerSandbox(cmd, set, candidates, unreachable, cpPickEmpty, cpPickAmbiguous)
	}
	if id := idpkg.Canonical(idpkg.PrefixSandbox, reference); idpkg.IsGenerated(id) {
		s, _, sandboxID, _, err := a.findOnEveryServer(cmd.Context(), set, id)
		return s, sandboxID, err
	}
	// configuredName, as in resolveCPSandbox.
	if s, sandboxID, ok, err := matchServerSandboxArg(reference, candidates, configuredName); err != nil || ok {
		return s, sandboxID, err
	}
	if !isResolvableShortID(idpkg.Canonical(idpkg.PrefixSandbox, reference)) {
		return nil, "", unmatchedSandboxName(reference)
	}
	s, _, sandboxID, _, err := a.findOnEveryServer(cmd.Context(), set, reference)
	if err != nil {
		return nil, "", unmatchedSandboxReference(reference, err)
	}
	return s, sandboxID, nil
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

type copyOptions struct{ recursive, preserve, quiet bool }

func parseCopyArgs(args []string) (copyOptions, []string, error) {
	var options copyOptions
	var paths []string
	for i, arg := range args {
		if arg == "--" {
			return options, append(paths, args[i+1:]...), nil
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			paths = append(paths, arg)
			continue
		}
		for _, flag := range arg[1:] {
			switch flag {
			case 'r':
				options.recursive = true
			case 'p':
				options.preserve = true
			case 'q':
				options.quiet = true
			default:
				return options, nil, fmt.Errorf("unsupported copy option %q: copy uses the files API; supported syntax: cp [-r] [-p] [-q] [--] SRC... DST", arg)
			}
		}
	}
	return options, paths, nil
}

func (a *App) resolveCPOperands(cmd *cobra.Command, client *apiclientgen.Client, projectID string, operands []cpOperand, resolved map[string]string) ([]cpOperand, error) {
	operands = slices.Clone(operands)
	if resolved == nil {
		resolved = map[string]string{}
	}
	for i := range operands {
		if !operands[i].remote {
			continue
		}
		id, seen := resolved[operands[i].reference]
		if !seen {
			var err error
			id, err = a.resolveCPSandbox(cmd, client, projectID, operands[i].reference)
			if err != nil {
				return nil, err
			}
			resolved[operands[i].reference] = id
		}
		operands[i].reference = id
		if operands[i].path == "" {
			operands[i].path = "~"
		}
	}
	return operands, nil
}
