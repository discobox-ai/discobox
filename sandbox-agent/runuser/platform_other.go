//go:build !linux

package runuser

import (
	"runtime"

	"github.com/discobox-ai/discobox/sandboxuser"
)

// Identity is every field a run identity has on this platform, which is what
// a caller that launches a process and describes it asks Resolve for. Off
// Linux a sandbox has one account and no POSIX ids, so an identity is a name
// and a home and nothing to setuid to (ADR 0145 §5).
const Identity = sandboxuser.FieldName | sandboxuser.FieldHome

// Current is the image layer: this process's own account. See currentAccount.
func Current() *User { return currentAccount() }

// Resolve answers with the sandbox's one account, refusing a layer that names
// anything else. See resolveOneAccount.
func Resolve(l Layers, need Fields) (User, error) {
	return resolveOneAccount(runtime.GOOS, l, need)
}
