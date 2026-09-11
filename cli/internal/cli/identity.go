package cli

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
)

// identityPair is what `discobox id` answers: the two IDs an enrollment is
// made of, each in both spellings so `-o json` never loses one.
type identityPair struct {
	Client identityValue `json:"client"`
	Server identityValue `json:"server"`
}

// identityValue is one identity, or the reason there is none.
//
// A missing half never suppresses the other: the client's ID is what somebody
// is about to enroll, and it is worth printing while the server is unreachable
// — which is often exactly when they are asking.
type identityValue struct {
	PeerID string `json:"peerId,omitempty"`
	// IrohEndpointID is the same value in iroh's own hex, which appears in
	// iroh's tracing and nowhere in Discobox (ADR 0098 §5).
	IrohEndpointID string `json:"irohEndpointId,omitempty"`
	// Source says where the ID came from: this machine's key file, the address
	// in --server, or the server itself. An ID read out of an address was
	// never confirmed by the server that answers to it, and a reader comparing
	// two IDs should know which of theirs is which.
	Source string `json:"source,omitempty"`
	// Reason says why there is no ID.
	Reason string `json:"reason,omitempty"`
	// Failed separates the two ways there can be none. A server that answered
	// "I have no peer ID" answered: it does not listen on discobox://, which is
	// the ordinary configuration and not a problem. A server that could not be
	// asked did not answer. Only the second is worth a non-zero exit.
	Failed bool `json:"failed,omitempty"`
}

func newIdentityValue(id endpoint.IrohID, source string) identityValue {
	return identityValue{PeerID: id.String(), IrohEndpointID: id.IrohEndpointID(), Source: source}
}

// identityFailure is a half that could not be obtained, as opposed to one that
// is legitimately absent.
func identityFailure(err error) identityValue {
	return identityValue{Reason: err.Error(), Failed: true}
}

// newIDCommand prints the two IDs an enrollment is made of.
//
// They are two halves of one setup step, printed together rather than one in a
// command and the other in a line of the server's startup log (ADR 0098).
// Printing them together is the point: what a person does with
// these is compare them against what the other machine says.
func (a *App) newIDCommand() *cobra.Command {
	var irohForm bool
	cmd := &cobra.Command{
		Use:   "id",
		Short: "Print this machine's peer ID and the server's",
		Long: "Print the two peer IDs an enrollment is made of: this machine's, which a server\n" +
			"admits, and the server's, which a client dials as discobox://<peer-id>.\n\n" +
			"This machine's ID is generated on first use and kept in the CLI state directory.\n" +
			"The server's is read from --server when that address names a peer, and asked of\n" +
			"the server otherwise.\n\n" +
			"`discobox admin peer id` prints this machine's ID alone, for a script, and needs\n" +
			"no server.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runID(cmd, irohForm)
		},
	}
	// Not the default, and not printed beside the peer ID: two spellings of one
	// identity in one output is what a single written form exists to avoid
	// (ADR 0097 §5, ADR 0098 §5). The JSON carries both because a program
	// reading it is not comparing them by eye.
	cmd.Flags().BoolVar(&irohForm, "iroh", false, "Print iroh's own hex form of each ID, for reading beside iroh's tracing; the JSON output always carries both")
	return cmd
}

func (a *App) runID(cmd *cobra.Command, irohForm bool) error {
	pair := identityPair{
		Client: a.clientIdentity(cmd),
		Server: a.serverIdentity(cmd.Context()),
	}
	if a.output == "json" {
		if err := writeJSON(cmd.OutOrStdout(), pair); err != nil {
			return err
		}
		return identityExit(pair)
	}

	if err := writeIdentityPair(cmd.OutOrStdout(), pair, irohForm); err != nil {
		return err
	}
	return identityExit(pair)
}

// writeIdentityPair prints the two rows: one label, one value each. Both are
// always printed, including a half that has no ID, because "there is no server
// ID, and here is why" is the answer in that case rather than the absence of
// one.
func writeIdentityPair(out io.Writer, pair identityPair, irohForm bool) error {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	for _, row := range []struct {
		label string
		value identityValue
	}{{"client", pair.Client}, {"server", pair.Server}} {
		fmt.Fprintf(writer, "%s\t%s\n", row.label, identityText(row.value, irohForm))
	}
	return writer.Flush()
}

// identityText is the one thing printed for an identity: the ID in the form
// that was asked for, or the reason there is none.
func identityText(value identityValue, irohForm bool) string {
	switch {
	case value.PeerID == "":
		return "-  " + value.Reason
	case irohForm:
		return value.IrohEndpointID
	default:
		return value.PeerID
	}
}

// identityExit fails when a half could not be obtained, so a script does not
// silently proceed with one of the two. A server that answered that it has no
// peer ID is not a failure: most servers are reached over a local socket and
// have none, and there is nothing wrong with them.
//
// The message is short because the rows above it have already said which half
// and why.
func identityExit(pair identityPair) error {
	switch {
	case pair.Client.Failed:
		return fmt.Errorf("this machine's peer ID could not be read: %s", pair.Client.Reason)
	case pair.Server.Failed:
		return fmt.Errorf("the server's peer ID could not be read: %s", pair.Server.Reason)
	default:
		return nil
	}
}

// clientIdentity is this machine's ID, generated on first use exactly as
// `discobox admin peer id` generates it — the file is the identity, and two
// commands that made different ones would be two machines.
func (a *App) clientIdentity(cmd *cobra.Command) identityValue {
	path := defaultIrohIdentityPath()
	id, created, err := loadOrCreateIrohIdentity(path)
	if err != nil {
		return identityFailure(err)
	}
	if created {
		fmt.Fprintf(cmd.ErrOrStderr(), "generated a new iroh identity at %s\n", path)
	}
	return newIdentityValue(id, path)
}

// serverIdentity is the server's ID, from the address when it names one and
// from the server otherwise.
//
// The address first, because it costs no round trip and answers while the
// server is unreachable — and because when a `discobox://` address is what the
// caller passed, that is the ID they are asking about.
func (a *App) serverIdentity(ctx context.Context) identityValue {
	parsed := a.serverEndpoint()
	if parsed.Scheme == "iroh" && parsed.Value != "" {
		if id, err := parsed.IrohID(); err == nil {
			return newIdentityValue(id, "--server")
		}
	}
	// No autolaunch: `discobox id` reports who this server is, and starting one
	// to answer that is not the same question. The same rule `status` follows.
	baseURL, httpClient, err := a.httpClientWithAutoStart(false)
	if err != nil {
		return identityFailure(err)
	}
	client, err := apiclientgen.NewClient(baseURL, apiclientgen.WithClient(httpClient))
	if err != nil {
		return identityFailure(err)
	}
	res, err := client.GetServerPeer(ctx)
	if err != nil {
		return identityFailure(err)
	}
	body, err := expectResponse[apimodel.ServerPeer](res)
	if err != nil {
		return identityFailure(err)
	}
	value, ok := body.GetPeerId().Get()
	if !ok || value == "" {
		// Answered, and the answer is none. Not a failure: a server listens on
		// discobox:// only when it was asked to.
		return identityValue{
			Source: "the server",
			Reason: "this server does not listen on discobox://, so it has no peer ID",
		}
	}
	id, err := endpoint.ParseIrohID(value)
	if err != nil {
		return identityFailure(fmt.Errorf("the server reported an unreadable peer ID %q: %w", value, err))
	}
	return newIdentityValue(id, "the server")
}
