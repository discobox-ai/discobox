// Package relay embeds the guest control-plane relay so the Windows server
// ships as a single binary.
//
// The relay is a Linux program that must run inside the pool guest, which has
// no Go toolchain and — on first connect — no guaranteed network. Cross
// compiling it on the host and carrying it inside the server binary avoids both
// problems. The driver then streams it into the guest over a guest process's
// stdin, so nothing here is ever written to a Windows directory or shared into
// the VM: the relay is the only program the guest needs, and it is also what
// backs every other guest connection (see its --dial mode).
//
// The payload is gzipped: the relay is ~2.4 MB stripped and ~1.0 MB compressed,
// nearly all of which is the Go runtime floor rather than anything the relay
// itself pulls in.
//
// The compressed binary is a build artifact, not source. `task build:cp-relay`
// produces it; the artifacts directory's committed README keeps `go build ./...`
// working in a fresh checkout with no artifact present, and Binary reports a
// clear error rather than handing back a truncated file if the build step has
// not run.
package relay

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"runtime"
)

// The whole directory is embedded rather than the artifact alone: go:embed
// requires its target to exist, but the artifact is gitignored so a built tree
// stays clean. The committed README keeps the directory — and therefore a plain
// `go build ./...` in a fresh checkout — working with no artifact present.
//
//go:embed artifacts
var artifacts embed.FS

// artifactName is the compressed relay produced by `task build:cp-relay` for
// this binary's architecture.
//
// WSL2 does not emulate, so a guest runs the architecture its Windows host
// does and the relay a server needs is the one matching the server itself.
// One source tree builds both Windows binaries, so the choice cannot be a
// constant: build:cp-relay produces a relay per architecture and each binary
// reads its own.
func artifactName() string {
	return "artifacts/discobox-cp-relay.linux-" + runtime.GOARCH + ".gz"
}

// relayGzip returns the embedded artifact, or nil when it was never built.
func relayGzip() []byte {
	data, err := artifacts.ReadFile(artifactName())
	if err != nil {
		return nil
	}
	return data
}

// GuestPath is where the driver installs the relay inside the guest. It is
// under /tmp, which is tmpfs and wiped when the VM restarts - the same
// lifetime as the session that installed it, so a rebooted guest never runs a
// relay left by an older server.
const GuestPath = "/tmp/discobox-cp-relay"

// ErrNotBuilt reports that the server was built without the guest relay
// artifact, so no pool can start.
var ErrNotBuilt = errors.New("wslc: guest control-plane relay was not built into this binary; run `task build:cp-relay`")

// minimumSize guards against a truncated or otherwise bogus artifact being
// mistaken for a real binary.
const minimumSize = 64 * 1024

// Available reports whether a usable relay is embedded.
func Available() bool { return len(relayGzip()) >= 512 }

// Binary returns the relay program, decompressed and ready to be streamed
// into a guest.
func Binary() ([]byte, error) {
	if !Available() {
		return nil, ErrNotBuilt
	}
	binary, err := decompress()
	if err != nil {
		return nil, err
	}
	if len(binary) < minimumSize {
		return nil, fmt.Errorf("%w (embedded artifact is only %d bytes)", ErrNotBuilt, len(binary))
	}
	return binary, nil
}

// Digest identifies the embedded relay, for logging which build a guest is
// running.
func Digest() string {
	sum := sha256.Sum256(relayGzip())
	return hex.EncodeToString(sum[:])[:12]
}

func decompress() ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(relayGzip()))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotBuilt, err)
	}
	defer func() { _ = reader.Close() }()
	binary, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("decompress relay: %w", err)
	}
	return binary, nil
}
