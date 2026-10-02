package layout

import "golang.org/x/sys/windows"

// privileged reports whether this process holds an elevated token. An
// administrator's ordinary, filtered token is not privileged: that is how a
// Windows administrator runs everything that was not explicitly elevated.
func privileged() bool { return windows.GetCurrentProcessToken().IsElevated() }
