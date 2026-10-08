package sandboxcreate

import (
	"github.com/discobox-ai/discobox/platform"
)

// linuxSandbox is the platform these tests place sources into unless they say
// otherwise: what every pool hosts today.
var linuxSandbox = platform.Platform{OS: "linux", Arch: "amd64"}
