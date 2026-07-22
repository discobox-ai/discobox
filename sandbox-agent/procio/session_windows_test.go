//go:build windows

package procio

import "syscall"

// newSessionAttr has no session to create on Windows.
func newSessionAttr() *syscall.SysProcAttr { return nil }
