package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/discobox-ai/discobox/pkg/version"
)

// versionServerTimeout bounds how long a version waits for the server to say
// which one it is — the dial included, not only the request, since over iroh
// bringing the transport up is most of the wait. Long enough for that dial to
// find its relay; short enough that a peer that is switched off does not hold
// the command open. The client's own line is printed before the wait starts.
const versionServerTimeout = 5 * time.Second

// newVersionCommand is `discobox version`, which prints what `discobox
// --version` prints.
//
// Anything driving a CLI without reading its help reaches for both spellings,
// and one of them answering "unknown command" is a worse trade than a command
// nobody has to be told about. It is listed, as -v/--version is: a spelling
// that works is one the help shows (cli/DESIGN.md, "Command Naming").
func (a *App) newVersionCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the client and server versions",
		Long: `Print this client's version and the version of the server --server names.
The server is asked, never started; one that does not answer is reported as
unavailable, and the command still succeeds.

With -o json it writes one object, {"client": ..., "server": ...}: "server" is
the version the server reported, or null when none answered, and --client
leaves it out.`,
		Args: cobra.NoArgs,
		// The root's hook resolves the leader key and starts the parent watch,
		// and a version needs neither. What somebody diagnosing a broken
		// environment asks first is what they are running, so an environment
		// this cannot parse — a leader key it does not know, an output format
		// it does not have — must not be what stops them hearing it. cobra runs
		// the closest hook only, so this one is how the root's is skipped; the
		// root's own hook lets --version through for the same reason.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			a.errOut = cmd.ErrOrStderr()
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.printVersion(cmd)
		},
	}
	cmd.Flags().Bool("client", false, "Print only the client version without contacting the server")
	return cmd
}

// printVersion writes what `discobox --version` and `discobox version` write:
// this binary's version, and the version of the server --server names.
//
// The server is asked, never started. A version that launched one would
// report a server that was not running a moment ago — downloading it first, on
// a machine with none staged — to answer a question about what is there. When
// nothing answers, the server's line says "unavailable" and the command still
// succeeds: an unreachable server is often why somebody is asking.
//
// Under -o json it writes one object, {"client": …, "server": …}: "server" is
// what the server reported, "" for one that answered without saying, and
// null for none answering; --client leaves the key out. Any other format is
// the text, since the version is what is read when an environment is broken
// (newVersionCommand).
func (a *App) printVersion(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	clientOnly, _ := cmd.Flags().GetBool("client")
	asJSON := a.output == "json"
	value := map[string]any{"client": version.String()}
	if !asJSON {
		// The client's line first, on a write of its own. This half was known
		// before the process started, and holding it back for a peer that is
		// switched off would put five seconds of silence in front of the
		// answer somebody diagnosing a broken environment came for. JSON is
		// one object, so it waits for the server's half.
		if _, err := fmt.Fprintf(out, "client version %s\n", version.String()); err != nil {
			return err
		}
	}
	if !clientOnly {
		// The server is whichever the root's hook would have settled on, but
		// a registry that does not parse is not a reason to withhold the
		// answer: the server named as it was is asked instead.
		_ = a.resolveServerName(serverChosen(cmd))
		answered, reported, hint := a.serverVersion(cmd.Context())
		if !asJSON {
			_, err := fmt.Fprintf(out, "server version %s%s\n", serverVersionText(answered, reported), hint)
			return err
		}
		value["server"] = nil
		if answered {
			value["server"] = strings.TrimSpace(reported)
		}
	}
	if !asJSON {
		return nil
	}
	// A server's version is the server's text, not this binary's, so it goes
	// to the terminal escaped.
	return writeTerminalSafeJSON(out, value)
}

// serverVersion is whether the server at --server answered, and the version
// it reports on its health endpoint, reached over the transport every other
// command uses — relays and all — with autolaunch off. hint is
// localServerHint's, for a local endpoint nothing is listening on; only the
// text form writes it.
//
// The deadline covers the whole attempt rather than the request alone. Building
// the client is what dials: over iroh it prepares this machine's identity and
// binds an endpoint, on a context of its own and before there is a request to
// cancel, so a deadline around the request would be a deadline around the fast
// part. A dial that outlasts the deadline is left to a goroutine this process
// is about to exit out from under, which is why the channel is buffered.
func (a *App) serverVersion(ctx context.Context) (answered bool, reported, hint string) {
	ctx, cancel := context.WithTimeout(ctx, versionServerTimeout)
	defer cancel()
	type answer struct {
		answered bool
		reported string
		hint     string
	}
	answers := make(chan answer, 1)
	go func() {
		baseURL, client, err := a.httpClientWithAutoStart(false)
		if err != nil {
			answers <- answer{}
			return
		}
		status, err := endpoint.ProbeHealth(ctx, baseURL, client)
		if err != nil && endpoint.ListenerGone(err) {
			answers <- answer{hint: a.localServerHint(ctx)}
			return
		}
		answers <- answer{answered: err == nil, reported: status.Version}
	}()
	select {
	case got := <-answers:
		return got.answered, got.reported, got.hint
	case <-ctx.Done():
		return false, "", ""
	}
}

// localServerHint says why a local server did not answer, when the machine
// says so: one that is not downloaded yet, or one that is there and not
// running, and what starts it. Right after an install both are expected, and
// a bare "unavailable" read as an error.
//
// It is only for the local endpoint, the one this CLI can stage and start: a
// remote server that does not answer is not fixed by downloading one here. It
// reads the disk and nothing else — a version neither downloads nor starts a
// server (printVersion). Only a probe that found nothing listening
// (endpoint.ListenerGone) gets it: one that timed out, was refused permission,
// or was answered by something that is not a Discobox server found something
// there, and "not running" would be false.
func (a *App) localServerHint(ctx context.Context) string {
	if !a.serverEndpoint().AutoLaunchable() {
		return ""
	}
	download, known := a.serverResolver(nil).needsDownload(ctx)
	if !known {
		return ""
	}
	autoLaunch := shouldAutoLaunchServer(a.autoStart)
	switch {
	case download && autoLaunch:
		return ": not downloaded yet (the first command that needs it downloads it, or run `discobox admin server stage`)"
	case download:
		return ": not downloaded yet (download it with `discobox admin server stage`, start it with `discobox admin server`)"
	case autoLaunch:
		return ": not running (the first command that needs it starts it)"
	default:
		return ": not running (start it with `discobox admin server`)"
	}
}

// serverVersionText is how a server's version is written for a reader: what it
// reported, "unknown" for a server that answered without saying, and
// "unavailable" when no server answered at all. `admin server status` writes it
// the same way, without localServerHint: its connect row carries its own
// remedy for a local endpoint nothing is listening on.
func serverVersionText(answered bool, reported string) string {
	switch {
	case !answered:
		return "unavailable"
	case strings.TrimSpace(reported) == "":
		return "unknown"
	default:
		return reported
	}
}
