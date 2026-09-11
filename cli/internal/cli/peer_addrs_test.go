package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

func TestPeerAddrsRoundTrip(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peer := testPeerID(t)

	if got := cachedPeerAddrs(peer); got != nil {
		t.Fatalf("cachedPeerAddrs on a fresh state dir = %v, want nothing", got)
	}
	rememberPeerAddrs(peer, []string{"192.168.1.185:56290"})
	if got := cachedPeerAddrs(peer); !slices.Equal(got, []string{"192.168.1.185:56290"}) {
		t.Fatalf("cachedPeerAddrs = %v, want the address the peer answered on", got)
	}

	// A peer that moved is remembered where it is now, not where it was.
	rememberPeerAddrs(peer, []string{"192.168.1.185:41551"})
	if got := cachedPeerAddrs(peer); !slices.Equal(got, []string{"192.168.1.185:41551"}) {
		t.Fatalf("cachedPeerAddrs = %v, want the latest address", got)
	}

	// Peers must not collide: the file holds one entry each.
	other := testPeerID(t)
	if got := cachedPeerAddrs(other); got != nil {
		t.Fatalf("cachedPeerAddrs for an unknown peer = %v, want nothing", got)
	}
}

// The memory is a hint, so nothing about it may fail a command: no state
// directory to write, a file full of garbage, an entry naming no address.
func TestPeerAddrsSurvivesUnusableState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	peer := testPeerID(t)

	rememberPeerAddrs(peer, nil)
	if got := cachedPeerAddrs(peer); got != nil {
		t.Fatalf("cachedPeerAddrs after recording nothing = %v, want nothing", got)
	}

	path := peerAddrsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := cachedPeerAddrs(peer); got != nil {
		t.Fatalf("cachedPeerAddrs over a corrupt file = %v, want nothing", got)
	}
	// And a corrupt file is replaced rather than being a permanent blind spot.
	rememberPeerAddrs(peer, []string{"127.0.0.1:1234"})
	if got := cachedPeerAddrs(peer); !slices.Equal(got, []string{"127.0.0.1:1234"}) {
		t.Fatalf("cachedPeerAddrs = %v, want the recorded address", got)
	}
}

// A server binds a fresh port every start, so an old entry is a guess that has
// almost certainly gone wrong. It stops being offered rather than being probed
// at forever.
func TestPeerAddrsExpire(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peer := testPeerID(t)

	entries := map[string]peerAddrsEntry{
		peer.Key(): {Addrs: []string{"10.0.0.1:1"}, At: time.Now().UTC().Add(-peerAddrsLifetime - time.Hour)},
	}
	if err := writeStateFile(peerAddrsPath(), entries); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
	if got := cachedPeerAddrs(peer); got != nil {
		t.Fatalf("cachedPeerAddrs for an expired entry = %v, want nothing", got)
	}

	// And the next write clears it out rather than carrying it forever.
	rememberPeerAddrs(testPeerID(t), []string{"10.0.0.2:2"})
	if _, ok := loadPeerAddrs()[peer.Key()]; ok {
		t.Fatal("an expired entry survived a write, so the file grows without bound")
	}
}

func TestPeerAddrsAreBounded(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for i := 0; i < peerAddrsLimit+10; i++ {
		rememberPeerAddrs(testPeerID(t), []string{"10.0.0.1:1"})
	}
	if got := len(loadPeerAddrs()); got > peerAddrsLimit {
		t.Fatalf("the file holds %d entries, want at most %d", got, peerAddrsLimit)
	}

	// One peer's list is bounded too: every address in it is a probe on the
	// next dial, and a long list is a history rather than a hint.
	peer := testPeerID(t)
	many := make([]string, 0, peerAddrsPerPeer+3)
	for i := 0; i < peerAddrsPerPeer+3; i++ {
		many = append(many, "10.0.0.1:"+strconv.Itoa(1000+i))
	}
	rememberPeerAddrs(peer, many)
	if got := len(cachedPeerAddrs(peer)); got > peerAddrsPerPeer {
		t.Fatalf("one peer has %d addresses, want at most %d", got, peerAddrsPerPeer)
	}
}

// Recording runs once per connection, so the common case — nothing has changed
// — must not rewrite the file.
func TestPeerAddrsDoesNotRewriteUnchanged(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peer := testPeerID(t)

	rememberPeerAddrs(peer, []string{"192.168.1.185:56290"})
	before, err := os.Stat(peerAddrsPath())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	data, err := os.ReadFile(peerAddrsPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	rememberPeerAddrs(peer, []string{"192.168.1.185:56290"})
	after, err := os.Stat(peerAddrsPath())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// SameFile rather than the modification time: writeStateFile renames a
	// temporary file into place, so a rewrite is a different file even when a
	// coarse filesystem clock gives it the same timestamp.
	if !os.SameFile(before, after) {
		t.Fatal("recording an unchanged address rewrote the file")
	}
	var entries map[string]peerAddrsEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !slices.Equal(entries[peer.Key()].Addrs, []string{"192.168.1.185:56290"}) {
		t.Fatalf("entry = %v, want the recorded address", entries[peer.Key()].Addrs)
	}
}

// A server that stays put reports the same address on every connection. Its
// entry must be kept alive by that, or it expires peerAddrsLifetime after the
// address last changed — exactly the server the memory is for.
func TestPeerAddrsKeepsAnUnchangedEntryAlive(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peer := testPeerID(t)
	addrs := []string{"192.168.1.185:56290"}

	entries := map[string]peerAddrsEntry{
		peer.Key(): {Addrs: addrs, At: time.Now().UTC().Add(-peerAddrsLifetime - time.Hour)},
	}
	if err := writeStateFile(peerAddrsPath(), entries); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
	if got := cachedPeerAddrs(peer); got != nil {
		t.Fatalf("cachedPeerAddrs for an expired entry = %v, want nothing", got)
	}

	// The next connection finds the server where it always was.
	rememberPeerAddrs(peer, addrs)
	if got := cachedPeerAddrs(peer); !slices.Equal(got, addrs) {
		t.Fatalf("cachedPeerAddrs after the same address answered again = %v, want %v", got, addrs)
	}
}

// A cached address that does not parse would fail the whole dial, because the
// transport rejects a malformed direct address outright. The memory may only
// ever add candidates, so it is dropped and the rest are kept.
func TestPeerAddrsDropsAnAddressThatDoesNotParse(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	peer := testPeerID(t)

	entries := map[string]peerAddrsEntry{
		peer.Key(): {Addrs: []string{"not an address", "192.168.1.185:56290"}, At: time.Now().UTC()},
	}
	if err := writeStateFile(peerAddrsPath(), entries); err != nil {
		t.Fatalf("writeStateFile: %v", err)
	}
	if got := cachedPeerAddrs(peer); !slices.Equal(got, []string{"192.168.1.185:56290"}) {
		t.Fatalf("cachedPeerAddrs = %v, want only the address that parses", got)
	}
}

func TestPeerAddrsLiveUnderXDGStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	rememberPeerAddrs(testPeerID(t), []string{"127.0.0.1:1"})

	want := filepath.Join(state, "discobox", "cli", "iroh", "peer-addrs.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("Stat(%s) = %v, want the memory beside the iroh identity", want, err)
	}
}

func testPeerID(t *testing.T) endpoint.IrohID {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	id, err := endpoint.IrohIDFromPublicKey(pub)
	if err != nil {
		t.Fatalf("IrohIDFromPublicKey: %v", err)
	}
	return id
}
