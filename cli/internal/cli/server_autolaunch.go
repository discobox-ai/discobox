package cli

import (
	"fmt"
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
// autolaunching, before --auto-start-server gets its say.
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

// autoStartServer is --auto-start-server, a three-way answer rather than a
// plain bool: the build and the environment already decide whether an
// invocation may autolaunch a server (autoLaunchConfigured), and a bool flag
// could only ever veto that, never override it the other way. "auto" — the
// default, and what an invocation that never mentions the flag gets — leaves
// their answer standing; "true" and "false" both outrank it, in the direction
// their name says.
//
// This is what lets a development build be told to launch one without
// resorting to DISCOBOX_SERVER_AUTOLAUNCH: `--auto-start-server=true` starts
// one for this invocation alone, `--auto-start-server=false` (or the bare
// flag, which used to be `--no-start`) refuses to, and the development
// default — off — is unchanged for anyone who never passes the flag.
type autoStartServer string

const (
	autoStartServerAuto  autoStartServer = "auto"
	autoStartServerTrue  autoStartServer = "true"
	autoStartServerFalse autoStartServer = "false"
)

func (m *autoStartServer) String() string { return string(*m) }
func (m *autoStartServer) Type() string   { return "true|false|auto" }

func (m *autoStartServer) Set(value string) error {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", string(autoStartServerAuto):
		*m = autoStartServerAuto
	case string(autoStartServerTrue), "yes", "y", "1":
		*m = autoStartServerTrue
	case string(autoStartServerFalse), "no", "n", "0":
		*m = autoStartServerFalse
	default:
		return fmt.Errorf("must be true, false, or auto")
	}
	return nil
}
