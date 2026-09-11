package irohd

import (
	"bytes"
	"log"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSocketsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if got := LoadSockets(dir); got != nil {
		t.Fatalf("LoadSockets on a fresh data dir = %v, want nothing", got)
	}
	sockets := []netip.AddrPort{
		netip.MustParseAddrPort("0.0.0.0:46966"),
		netip.MustParseAddrPort("[::]:54867"),
	}
	if err := RememberSockets(dir, sockets); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	if got := LoadSockets(dir); !slices.Equal(got, sockets) {
		t.Fatalf("LoadSockets = %v, want %v", got, sockets)
	}

	// A server that came back somewhere else is remembered where it is now.
	moved := []netip.AddrPort{netip.MustParseAddrPort("0.0.0.0:41551")}
	if err := RememberSockets(dir, moved); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	if got := LoadSockets(dir); !slices.Equal(got, moved) {
		t.Fatalf("LoadSockets = %v, want %v", got, moved)
	}
}

// The sockets offered replace the endpoint's defaults, so a file that is only
// partly usable must offer nothing rather than half an endpoint.
func TestLoadSocketsRefusesAFileItCannotReadWhole(t *testing.T) {
	for name, contents := range map[string]string{
		"garbage line": "0.0.0.0:46966\nnot a socket\n",
		"zero port":    "0.0.0.0:0\n",
		"empty":        "",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, socketsFileName), []byte(contents), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if got := LoadSockets(dir); got != nil {
				t.Fatalf("LoadSockets = %v, want nothing", got)
			}
		})
	}
}

// Every start reports its sockets, and the common case is the same ones as last
// time, which must not rewrite the file.
func TestRememberSocketsLeavesAnUnchangedFileAlone(t *testing.T) {
	dir := t.TempDir()
	sockets := []netip.AddrPort{netip.MustParseAddrPort("0.0.0.0:46966")}
	if err := RememberSockets(dir, sockets); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	path := filepath.Join(dir, socketsFileName)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if err := RememberSockets(dir, sockets); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// SameFile rather than the modification time: a rewrite renames a new file
	// into place, which a coarse filesystem clock can give the same timestamp.
	if !os.SameFile(before, after) {
		t.Fatal("remembering unchanged sockets rewrote the file")
	}
}

// Losing a port is logged because it is the restart every client pays for.
// Gaining a socket the last start could not bind kept every port a client could
// have learned, so it is recorded without a word.
func TestRememberSocketsLogsOnlyALostPort(t *testing.T) {
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(previous) })

	dir := t.TempDir()
	v4 := netip.MustParseAddrPort("0.0.0.0:46966")
	if err := RememberSockets(dir, []netip.AddrPort{v4}); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}

	gained := []netip.AddrPort{v4, netip.MustParseAddrPort("[::]:54867")}
	if err := RememberSockets(dir, gained); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	if logged.Len() != 0 {
		t.Fatalf("gaining an IPv6 socket logged %q, but no remembered port moved", logged.String())
	}
	if got := LoadSockets(dir); !slices.Equal(got, gained) {
		t.Fatalf("LoadSockets = %v, want the gained set %v recorded", got, gained)
	}

	moved := []netip.AddrPort{netip.MustParseAddrPort("0.0.0.0:41551"), netip.MustParseAddrPort("[::]:54867")}
	if err := RememberSockets(dir, moved); err != nil {
		t.Fatalf("RememberSockets: %v", err)
	}
	if !strings.Contains(logged.String(), v4.String()) {
		t.Fatalf("losing %v logged %q, want it named", v4, logged.String())
	}
	if strings.Contains(logged.String(), "[::]:54867 from") {
		t.Fatalf("logged %q, which names the IPv6 socket that did not move as lost", logged.String())
	}
}
