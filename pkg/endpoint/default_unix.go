//go:build !windows

package endpoint

import (
	"net/url"
	"os"
	"path/filepath"
	"strconv"
)

func DefaultEndpoint() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "discobox-"+strconv.Itoa(os.Getuid()))
	}
	// Rendered through net/url rather than concatenated: Parse reads the
	// decoded path back, so a runtime directory holding "#", "?" or "%" must
	// be escaped here or it comes back truncated or unparseable.
	return (&url.URL{Scheme: "unix", Path: filepath.Join(base, "discobox", "server.sock")}).String()
}
