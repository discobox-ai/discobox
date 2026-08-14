//go:build !linux

package buildkitagent

import (
	"context"
	"fmt"
)

// A per-build forwarder is built out of network namespaces, setns, and process
// signals. Those are Linux, and so is everything that would call these: the
// pool container, buildkitd, and the runc wrapper the hooks run in.
//
// They fail rather than doing nothing, because a build that silently ran with
// no forwarder would have unattributed egress — the exact thing this mechanism
// exists to prevent. The rest of the package still compiles everywhere, so the
// address and identity contract stays testable off Linux.

var errNotLinux = fmt.Errorf("per-build egress needs Linux network namespaces")

func StartBuildForwarder(context.Context, string, string, int) error { return errNotLinux }

func ServeBuildForwarder(context.Context, string, string, int) error { return errNotLinux }

func StopBuildForwarder(string) error { return errNotLinux }
