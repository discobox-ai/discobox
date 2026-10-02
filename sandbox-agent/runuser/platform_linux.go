package runuser

import "github.com/discobox-ai/discobox/sandboxuser"

// Identity is every field a run identity has on this platform, which is what
// a caller that launches a process and describes it asks Resolve for. On Linux
// that is all of them: a process runs by uid, gid and group set, and is
// described by a name and a home.
const Identity = sandboxuser.Complete

// Current is the image layer: who this process already is. See currentPOSIX.
func Current() *User { return currentPOSIX() }

// Resolve merges the layers by precedence and completes every field in need
// against the image's own account database. See resolvePOSIX.
func Resolve(l Layers, need Fields) (User, error) { return resolvePOSIX(l, need) }
