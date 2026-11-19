//go:build windows

package docker

import "context"

// socketAccess has nothing to say on Windows: the daemon is reached over a
// named pipe or TCP, neither of which is a socket with a group on it, and the
// client's own message for an unelevated client already names the pipe.
func socketAccess(context.Context, string) (refused bool, hint string) { return false, "" }
