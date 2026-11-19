//go:build windows

package docker

import "context"

// Windows Docker endpoints use named pipes or TCP.
func probeSocketAccess(context.Context, string) (bool, string) { return false, "" }
