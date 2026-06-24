//go:build !windows

package localipc

import (
	"os"
	"path/filepath"
	"strconv"
)

func DefaultEndpoint() string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "discobox-"+strconv.Itoa(os.Getuid()))
	}
	return "unix://" + filepath.Join(base, "discobox", "server.sock")
}
