package endpoint

import (
	"os"
	"strings"
)

// AddressEnv names the local server's endpoint outright, in place of the
// platform default below. Both ends read it through DefaultEndpoint — the
// server binds it and the CLI dials it — so it is the one setting that moves
// the rendezvous point without the two disagreeing.
//
// It exists for environments that rewrite what the default is derived from.
// Nix's devShell replaces TMPDIR with a fresh, short-lived directory per shell,
// so a server started in one shell is invisible from the next; the repository's
// flake pins this to the address a plain shell resolves. The default itself
// keeps following TMPDIR: on macOS that is the user's private launchd temp
// directory, and every released server before it listened there.
const AddressEnv = "DISCOBOX_SERVER_ADDRESS"

// DefaultEndpoint is the local server's endpoint: AddressEnv when set,
// otherwise this platform's default socket or pipe.
func DefaultEndpoint() string {
	if address := strings.TrimSpace(os.Getenv(AddressEnv)); address != "" {
		return address
	}
	return platformDefaultEndpoint()
}
