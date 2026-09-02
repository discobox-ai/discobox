// Package relay embeds the guest control-plane relay so the Windows server
// ships as a single binary.
//
// The relay is a Linux program that must run inside the pool guest, which has
// no Go toolchain and — on first connect — no guaranteed network. Cross
// compiling it on the host and carrying it inside the server binary avoids both
// problems, unlike the stdio bridge, which compiles itself in-guest using the
// guest's Docker daemon and therefore needs an image pull the first time.
//
// The payload is gzipped: the relay is ~2.4 MB stripped and ~1.0 MB compressed,
// nearly all of which is the Go runtime floor rather than anything the relay
// itself pulls in.
//
// The compressed binary is a build artifact, not source. `task build:cp-relay`
// produces it; the committed placeholder keeps `go build ./...` working in a
// fresh checkout, and Extract reports a clear error rather than writing a
// truncated file if the build step has not run.
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
	"os"
	"path/filepath"
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

// GuestPath is where the relay is mounted inside the guest.
const GuestPath = "/mnt/discobox-relay"

// BinaryName is the relay's file name, both on the host staging directory and
// under GuestPath.
const BinaryName = "discobox-cp-relay"

// ErrNotBuilt reports that the server was built without the guest relay
// artifact, so no pool can start.
var ErrNotBuilt = errors.New("wslc: guest control-plane relay was not built into this binary; run `task build:cp-relay`")

// minimumSize guards against the placeholder or a truncated artifact being
// mistaken for a real binary.
const minimumSize = 64 * 1024

// Available reports whether a usable relay is embedded.
func Available() bool { return len(relayGzip()) >= 512 }

// Extract writes the relay into dir and returns its path. The directory is
// mounted into the guest read-only, so the file is rewritten only when its
// contents differ, letting concurrent pools share one staging directory.
func Extract(dir string) (string, error) {
	if !Available() {
		return "", ErrNotBuilt
	}
	binary, err := decompress()
	if err != nil {
		return "", err
	}
	if len(binary) < minimumSize {
		return "", fmt.Errorf("%w (embedded artifact is only %d bytes)", ErrNotBuilt, len(binary))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create relay directory: %w", err)
	}
	path := filepath.Join(dir, BinaryName)
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, binary) {
		return path, nil
	}
	// Write to a unique temporary name and rename, so a pool starting
	// concurrently never mounts a half-written binary.
	tmp, err := os.CreateTemp(dir, "."+BinaryName+".*")
	if err != nil {
		return "", fmt.Errorf("stage relay: %w", err)
	}
	tmpPath := tmp.Name()
	installed := false
	defer func() {
		_ = tmp.Close()
		if !installed {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(binary); err != nil {
		return "", fmt.Errorf("write relay: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return "", fmt.Errorf("install relay: %w", err)
	}
	installed = true
	return path, nil
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
