//go:build !windows

package procio

import "syscall"

// newSessionAttr starts the process in a new session, as the sandbox does. That
// is what makes its process group orphaned, which is the condition the signal
// mapping is built around.
func newSessionAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
