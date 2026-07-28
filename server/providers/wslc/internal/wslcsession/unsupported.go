//go:build !windows

package wslcsession

import (
	"fmt"
	"net"
	"runtime"
)

// Session is a stub on every platform except Windows: wslc (WSL Containers)
// is a Windows-only feature, and this package's real implementation talks
// directly to Windows COM interfaces (ole32.dll, ws2_32.dll) that simply
// don't exist anywhere else. This file exists purely so the package
// compiles cleanly on non-Windows platforms - e.g. `go build ./...`/`go
// vet ./...` in a cross-platform CI matrix, or a larger project that
// imports wslcsession behind a runtime check rather than a build tag - not so
// that it does anything useful there. Every method returns
// errUnsupported immediately.
type Session struct{}

var errUnsupported = fmt.Errorf("wslcsession: not supported on %s (wslc requires Windows + WSL2)", runtime.GOOS)

// NewSession always fails on this platform. See the Session doc comment.
func NewSession(Options) (*Session, error) {
	return nil, errUnsupported
}

func (*Session) Close() error                           { return errUnsupported }
func (*Session) DisplayName() (string, error)           { return "", errUnsupported }
func (*Session) DockerConn() (net.Conn, error)          { return nil, errUnsupported }
func (*Session) DialGuestUnix(string) (net.Conn, error) { return nil, errUnsupported }
func (*Session) DialGuestTCP(string) (net.Conn, error)  { return nil, errUnsupported }

func (*Session) MountFolder(string, string, bool) error { return errUnsupported }

func (*Session) StartProcess(string, []string) (net.Conn, error) { return nil, errUnsupported }
