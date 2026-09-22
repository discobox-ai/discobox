//go:build !darwin || !cgo

package macvm

import (
	"context"
	"fmt"
	"runtime"
)

// The two reasons this build has no bindings. They are values rather than
// errors built where they are returned so that the entry points keep returning
// an opaque error: a function that can only return a concrete non-nil type
// makes every caller's `err != nil` provably true, which staticcheck reports on
// the caller rather than here (SA4023).
var (
	errNotMacOS = fmt.Errorf("%w: the framework is macOS-only", ErrUnsupported)
	errNoCgo    = fmt.Errorf("%w: the bindings are cgo and this binary was built with CGO_ENABLED=0; rebuild it with CGO_ENABLED=1", ErrUnsupported)
)

// unsupported names which of the two builds this is. The fix differs — use a
// Mac, or turn cgo back on — so the message has to say which one applies.
func unsupported() error {
	if runtime.GOOS == "darwin" {
		return errNoCgo
	}
	return errNotMacOS
}

// Supported reports that this build cannot run VMs.
func Supported() error { return unsupported() }

// LatestRestoreImageURL is unavailable without the bindings.
func LatestRestoreImageURL() (string, error) { return "", unsupported() }

// FetchRestoreImage is unavailable without the bindings.
func FetchRestoreImage(context.Context, string, func(float64, int64)) error { return unsupported() }

// Install is unavailable without the bindings.
func Install(context.Context, InstallOptions) error { return unsupported() }

// Run is unavailable without the bindings.
func Run(context.Context, RunOptions) error { return unsupported() }

// Clone is unavailable without the bindings.
func Clone(CloneOptions) error { return unsupported() }
