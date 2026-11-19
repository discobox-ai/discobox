package docker

import "strings"

// unixSocketPath is the filesystem path a Docker host names, or "" for a host
// that is not a Unix socket — a TCP daemon, or a Windows named pipe.
func unixSocketPath(host string) string {
	path, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		return ""
	}
	return path
}
