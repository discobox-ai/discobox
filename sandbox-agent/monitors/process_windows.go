//go:build windows

package monitors

import "os/exec"

// ownProcessGroup leaves the command as it is: a Windows sandbox has no
// process groups to end, and its monitors are ended with their process.
func ownProcessGroup(*exec.Cmd) {}
