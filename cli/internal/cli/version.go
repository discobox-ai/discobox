package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/version"
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
// nobody has to be told about. It is hidden rather than listed: --version is
// the spelling the help documents, and a command list is for the things you
// cannot guess.
func (a *App) newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "version",
		Short:  "Print the client and server versions",
		Hidden: true,
		Args:   cobra.NoArgs,
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
}

// printVersion writes what `discobox --version` and `discobox version` write:
// this binary's version, and the version of the server --server names.
//
// The server is asked, never started. A version that launched one would
// report a server that was not running a moment ago — downloading it first, on
// a machine with none staged — to answer a question about what is there. When
// nothing answers, the server's line says "unavailable" and the command still
// succeeds: an unreachable server is often why somebody is asking.
func (a *App) printVersion(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	// The client's line first, on a write of its own. This half was known
	// before the process started, and holding it back for a peer that is
	// switched off would put five seconds of silence in front of the answer
	// somebody diagnosing a broken environment came for.
	if _, err := fmt.Fprintf(out, "client version %s\n", version.String()); err != nil {
		return err
	}
	_, err := fmt.Fprintf(out, "server version %s\n", a.serverVersion(cmd.Context()))
	return err
}

// serverVersion is the version the server at --server reports on its health
// endpoint, reached over the transport every other command uses — relays and
// all — with autolaunch off.
//
// The deadline covers the whole attempt rather than the request alone. Building
// the client is what dials: over iroh it prepares this machine's identity and
// binds an endpoint, on a context of its own and before there is a request to
// cancel, so a deadline around the request would be a deadline around the fast
// part. A dial that outlasts the deadline is left to a goroutine this process
// is about to exit out from under, which is why the channel is buffered.
func (a *App) serverVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, versionServerTimeout)
	defer cancel()
	answered := make(chan string, 1)
	go func() {
		baseURL, client, err := a.httpClientWithAutoStart(false)
		if err != nil {
			answered <- serverVersionText(false, "")
			return
		}
		status, err := endpoint.ProbeHealth(ctx, baseURL, client)
		answered <- serverVersionText(err == nil, status.Version)
	}()
	select {
	case text := <-answered:
		return text
	case <-ctx.Done():
		return serverVersionText(false, "")
	}
}

// serverVersionText is how a server's version is written for a reader: what it
// reported, "unknown" for a server that answered without saying, and
// "unavailable" when no server answered at all. `admin server status` writes it
// the same way.
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
