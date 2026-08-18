//go:build windows

package procio

// WaitForLineEditor has nothing to wait for: this package runs no TTY process
// on Windows, so there is no line discipline and nothing echoes twice.
func (p *Process) WaitForLineEditor() bool { return true }
