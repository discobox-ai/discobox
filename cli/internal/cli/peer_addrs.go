package cli

import (
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

// A client remembers where a server answered, and offers those addresses back
// on the next dial alongside discovery.
//
// Discovery publishes a peer's relay and nothing else, so a client that knows
// only a peer ID has to reach the server through a relay before it can hole
// punch its way to a direct path. That bootstrap is one round trip through
// somebody else's infrastructure on a good day, and on a bad one it is the
// whole reason a command cannot connect: a measured relay path of 5.9s against
// 4ms for the same server dialed at its address makes two round trips of
// handshake the difference between connecting and timing out.
//
// The memory is a hint and nothing more. It is offered *beside* discovery,
// never instead of it, so an address that has gone stale costs a probe that
// nobody waits for — iroh dials every address it is given at once and takes
// whichever answers, and a peer dialed with a dead address and a live relay
// still connects. That is what makes it safe to keep an address that will be
// wrong the moment the server restarts.
//
// It is best-effort like the rest of the CLI's state (see statedir.go): a
// missing, unreadable, or corrupt file just means the next dial waits on
// discovery, which is what every dial did before this existed.

// peerAddrsFile is the state file, relative to the CLI's state directory. It
// sits beside the iroh identity because it is about the same transport, and
// under `iroh/` rather than at the top because it means nothing to any other.
const peerAddrsFile = "iroh/peer-addrs.json"

const (
	// peerAddrsLimit bounds the file so a long-lived install does not
	// accumulate an entry per server it has ever touched.
	peerAddrsLimit = 50
	// peerAddrsPerPeer bounds one peer's addresses. A connection settles on
	// one direct path and may hold a second; a list longer than this is a
	// history of where a server used to be, and every entry in it is a probe
	// on the next dial.
	peerAddrsPerPeer = 4
	// peerAddrsLifetime is how long an address is worth offering. A server
	// binds a fresh UDP port every start, so what is remembered here is
	// invalidated by an ordinary restart and is only ever a guess about a
	// server that has stayed put. Long enough to survive a laptop being shut
	// for a weekend, short enough that a machine that has moved is not
	// probed at for a month.
	peerAddrsLifetime = 14 * 24 * time.Hour
	// peerAddrsRefresh is how old an unchanged entry may get before recording
	// it again writes the file anyway. Without it an entry would expire
	// peerAddrsLifetime after the addresses last *changed*, which is exactly
	// the server that stayed put; with it, a peer costs one write a day rather
	// than one per connection.
	peerAddrsRefresh = 24 * time.Hour
)

// peerAddrsEntry is where one peer was last reached.
type peerAddrsEntry struct {
	// Addrs are socket addresses, the path the connection was using first.
	Addrs []string `json:"addrs"`
	// At is when they were last recorded — within peerAddrsRefresh of when the
	// peer last answered on them — and is what expiry and trimming sort on.
	At time.Time `json:"at"`
}

func peerAddrsPath() string {
	return filepath.Join(cliStateDir(), peerAddrsFile)
}

func loadPeerAddrs() map[string]peerAddrsEntry {
	data, err := os.ReadFile(peerAddrsPath())
	if err != nil {
		return nil
	}
	var entries map[string]peerAddrsEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil
	}
	return entries
}

// cachedPeerAddrs is the [endpoint.IrohConfig.Locate] half: the addresses to
// try for id beside whatever discovery finds.
//
// An address that does not parse is dropped here rather than handed on. The
// transport treats a malformed direct address as a hard error, which is right
// for an `?addr=` somebody typed and wrong for a hint: one bad entry would fail
// every dial to this peer until it expired, over a file nobody wrote by hand.
func cachedPeerAddrs(id endpoint.IrohID) []string {
	entry, ok := loadPeerAddrs()[id.Key()]
	if !ok || time.Since(entry.At) > peerAddrsLifetime {
		return nil
	}
	var addrs []string
	for _, addr := range entry.Addrs {
		if _, err := netip.ParseAddrPort(addr); err == nil {
			addrs = append(addrs, addr)
		}
	}
	return addrs
}

// rememberPeerAddrs is the [endpoint.IrohConfig.Reached] half: it records that
// id answered on addrs.
//
// It runs once for each connection that reaches a direct path, so the common
// case is that nothing has changed and the right thing to do is nothing —
// writing a file per command to say the server is still where it was would be
// a strange way to spend a disk. The exception is an entry old enough that it
// needs its time moved on (peerAddrsRefresh).
//
// Failures are dropped rather than reported. This is called from inside the
// transport while a command is running, and a command that worked is not
// going to be failed over a hint it could not write down.
func rememberPeerAddrs(id endpoint.IrohID, addrs []string) {
	if len(addrs) == 0 {
		return
	}
	if len(addrs) > peerAddrsPerPeer {
		addrs = addrs[:peerAddrsPerPeer]
	}
	key := id.Key()
	entries := loadPeerAddrs()
	if entries == nil {
		entries = map[string]peerAddrsEntry{}
	}
	if existing, ok := entries[key]; ok && slices.Equal(existing.Addrs, addrs) && time.Since(existing.At) < peerAddrsRefresh {
		return
	}
	entries[key] = peerAddrsEntry{Addrs: addrs, At: time.Now().UTC()}
	trimPeerAddrs(entries)
	_ = writeStateFile(peerAddrsPath(), entries)
}

// trimPeerAddrs drops expired entries, then the oldest of whatever is left
// over the limit.
func trimPeerAddrs(entries map[string]peerAddrsEntry) {
	for key, entry := range entries {
		if time.Since(entry.At) > peerAddrsLifetime {
			delete(entries, key)
		}
	}
	if len(entries) <= peerAddrsLimit {
		return
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return entries[keys[i]].At.After(entries[keys[j]].At) })
	for _, key := range keys[peerAddrsLimit:] {
		delete(entries, key)
	}
}
