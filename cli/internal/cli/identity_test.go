package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
)

// The two halves are printed together because comparing them is what a person
// does with them, and each is labeled so a pasted pair says which is which.
func TestIDPrintsBothHalves(t *testing.T) {
	client, server := testIdentityPair(t)
	printed := renderID(t, identityPair{Client: client, Server: server}, false)

	for _, want := range []string{"client", client.PeerID, "server", server.PeerID} {
		if !strings.Contains(printed, want) {
			t.Fatalf("output is missing %q:\n%s", want, printed)
		}
	}
	// The peer-ID form only. Two spellings of one identity in one output is
	// what a single written form exists to prevent (ADR 0098 §5).
	if strings.Contains(printed, client.IrohEndpointID) {
		t.Fatalf("the default output carries the hex form:\n%s", printed)
	}
}

// --iroh swaps the form rather than adding to it, so the output stays one
// identifier per machine.
func TestIDIrohFormReplacesThePeerID(t *testing.T) {
	client, server := testIdentityPair(t)
	printed := renderID(t, identityPair{Client: client, Server: server}, true)

	for _, want := range []string{client.IrohEndpointID, server.IrohEndpointID} {
		if !strings.Contains(printed, want) {
			t.Fatalf("output is missing %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, client.PeerID) {
		t.Fatalf("--iroh still carries the peer-ID form:\n%s", printed)
	}
}

// A server that does not listen for peers has answered the question. It is not
// a failure, because it is what most servers are: reached over a local socket,
// with no peer identity and nothing wrong.
func TestIDAcceptsAServerWithNoPeerID(t *testing.T) {
	client, _ := testIdentityPair(t)
	pair := identityPair{
		Client: client,
		Server: identityValue{Source: "the server", Reason: "this server gives no peer ID, which only a server from before every server had one does"},
	}
	if err := identityExit(pair); err != nil {
		t.Fatalf("identityExit() = %v, want nil for a server that answered", err)
	}
	printed := renderID(t, pair, false)
	if !strings.Contains(printed, "gives no peer ID") {
		t.Fatalf("the reason is missing from the row:\n%s", printed)
	}
}

// A server that could not be asked is a failure, so a script does not proceed
// with one of the two IDs it asked for.
func TestIDFailsWhenAHalfCouldNotBeRead(t *testing.T) {
	client, _ := testIdentityPair(t)
	pair := identityPair{Client: client, Server: identityFailure(errTestUnreachable)}
	if err := identityExit(pair); err == nil {
		t.Fatal("identityExit() = nil for a server that could not be asked")
	}
	// And the client half is still printed: it is what the reader is about to
	// enroll, and an unreachable server is often why they are asking.
	printed := renderID(t, pair, false)
	if !strings.Contains(printed, client.PeerID) {
		t.Fatalf("the client half was suppressed by the server half:\n%s", printed)
	}
}

// -o json carries both spellings whatever the terminal was shown, because a
// program reading it is not comparing them by eye.
func TestIDJSONCarriesBothForms(t *testing.T) {
	client, server := testIdentityPair(t)
	encoded, err := json.Marshal(identityPair{Client: client, Server: server})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var decoded identityPair
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded.Client.PeerID != client.PeerID || decoded.Client.IrohEndpointID != client.IrohEndpointID {
		t.Fatalf("the client half lost a form: %s", encoded)
	}
	if decoded.Server.PeerID != server.PeerID || decoded.Server.IrohEndpointID != server.IrohEndpointID {
		t.Fatalf("the server half lost a form: %s", encoded)
	}
}

// `discobox id` is top-level: it is half of the enrollment step, and the other
// half is printed by a server at startup, not by a command under `admin`.
func TestIDCommandIsTopLevel(t *testing.T) {
	root, _ := newRootCommand()
	found, _, err := root.Find([]string{"id"})
	if err != nil {
		t.Fatalf("Find(id) error = %v", err)
	}
	if found.Name() != "id" || found.Parent() != root {
		t.Fatalf("id resolved to %q under %v, want a top-level command", found.Name(), found.Parent())
	}
}

// An address that names a peer answers without a round trip, and says so: an
// ID read out of --server was never confirmed by the server that answers to it.
func TestServerIdentityReadsTheAddress(t *testing.T) {
	id := testIrohID(t, 0x11)
	app := &App{serverURL: endpoint.IrohURL(id), output: "table"}
	value := app.serverIdentity(t.Context())
	if value.PeerID != id.String() {
		t.Fatalf("PeerID = %q, want %q", value.PeerID, id.String())
	}
	if value.Source != "--server" {
		t.Fatalf("Source = %q, want the address it was read from", value.Source)
	}
}

// renderID prints a pair the way the command does, through the command's own
// writer rather than a copy of its loop.
func renderID(t *testing.T, pair identityPair, irohForm bool) string {
	t.Helper()
	var out bytes.Buffer
	if err := writeIdentityPair(&out, pair, irohForm); err != nil {
		t.Fatalf("writeIdentityPair() error = %v", err)
	}
	return out.String()
}

func testIdentityPair(t *testing.T) (client, server identityValue) {
	t.Helper()
	return newIdentityValue(testIrohID(t, 0xaa), "/state/iroh/id_ed25519"),
		newIdentityValue(testIrohID(t, 0xbb), "the server")
}

func testIrohID(t *testing.T, seed byte) endpoint.IrohID {
	t.Helper()
	var key [32]byte
	key[0] = seed
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey() error = %v", err)
	}
	return id
}

var errTestUnreachable = errTestError("dial unix /run/discobox/server.sock: no such file or directory")

type errTestError string

func (e errTestError) Error() string { return string(e) }
