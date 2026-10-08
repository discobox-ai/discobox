package sandboxruntime

import (
	"github.com/discobox-ai/discobox/platform"
	"github.com/discobox-ai/discobox/sandboxpath"
)

// linuxPaths are the path rules of the sandboxes these tests make: Linux, what
// a Docker pool hosts.
var linuxPaths = sandboxpath.For(platform.Platform{OS: "linux", Arch: "amd64"})
