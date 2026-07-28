//go:build windows

package wslcsession

import (
	"bytes"
	_ "embed"
	"os"
	"path/filepath"
)

// wslc's guest image ships no tool that can bridge a process's stdio to an
// arbitrary unix socket or TCP address (confirmed by exhaustively checking:
// no socat, ncat, python3, or perl among its ~650 binaries, and BusyBox's nc
// has no -U flag). bridgesrc/bridge.c (~150 lines) fills that gap.
//
// Rather than cross-compiling bridge.c on the Windows host (which needs
// *some* C toolchain reachable from Windows - Docker Desktop, a WSL distro
// with its own dockerd, or similar), these are embedded as plain source and
// a tiny shell wrapper (bridgesrc/entrypoint.sh): the first
// DockerConn/DialGuestUnix/DialGuestTCP call on a freshly booted session
// builds the binary once, using the *guest's own dockerd* - the same daemon
// this library talks to for everything else - and caches it under /tmp for
// the rest of that VM's lifetime. Every call after the first just execs the
// cached binary directly. No C toolchain or docker CLI is ever needed on the
// machine running this Go program.
//
//go:embed bridgesrc/bridge.c
var bridgeSource []byte

//go:embed bridgesrc/entrypoint.sh
var bridgeEntrypoint []byte

const guestBridgeMountPath = "/mnt/wslcsession-bridge"

func extractBridgeBinary() (string, error) {
	dir := filepath.Join(os.TempDir(), "wslcsession-bridge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	if err := writeIfChanged(filepath.Join(dir, "bridge.c"), bridgeSource, 0o644); err != nil {
		return "", err
	}
	if err := writeIfChanged(filepath.Join(dir, "entrypoint.sh"), bridgeEntrypoint, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func writeIfChanged(path string, content []byte, mode os.FileMode) error {
	existing, err := os.ReadFile(path)
	if err == nil && bytes.Equal(existing, content) {
		return nil
	}
	return os.WriteFile(path, content, mode)
}
