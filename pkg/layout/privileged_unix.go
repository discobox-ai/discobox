//go:build !windows

package layout

import "os"

// privileged reports whether this process runs as root.
func privileged() bool { return os.Geteuid() == 0 }
