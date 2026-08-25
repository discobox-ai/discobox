package cli

import (
	"os"
	"strconv"
	"strings"
)

// serverAutoLaunch is overridden to "true" by the release build's linker
// flags. Development and other ordinary builds retain the disabled default.
var serverAutoLaunch = "false"

// AutoLaunchEnv overrides that default, in either direction.
//
// It exists because the capability is otherwise a property of how the binary
// was linked, which makes the one thing you would want to try — does my change
// to the autolaunch work — reachable only by cutting a release build. A
// development binary sets this to test it; a release binary can set it to 0 to
// behave like a development one.
//
// Development builds still default to off, and that default is the point: a
// developer runs the server themselves under `task dev`, and a CLI that quietly
// forked a second one would race it for the same socket and the two would evict
// each other through the data directory's singleton lock.
const AutoLaunchEnv = "DISCOBOX_SERVER_AUTOLAUNCH"

// autoLaunchConfigured reports what the build and the environment say about
// autolaunching, before --no-start gets its say.
//
// An unparseable value is treated as unset rather than as an error: this
// decides whether a convenience happens, and failing a command outright over a
// typo in it would be a worse answer than not launching.
func autoLaunchConfigured() bool {
	if value := strings.TrimSpace(os.Getenv(AutoLaunchEnv)); value != "" {
		if enabled, err := strconv.ParseBool(value); err == nil {
			return enabled
		}
	}
	return serverAutoLaunch == "true"
}
