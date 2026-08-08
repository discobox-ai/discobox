package execs

import "github.com/obot-platform/discobox/sandbox-agent/runuser"

// Identity resolution lives in runuser, which owns it for the whole sandbox.
// These are the names the exec package used before the move; new code should
// call runuser directly.
var (
	ResolveNameAndHome = runuser.NameAndHome
	resolveGroups      = runuser.Groups
)
