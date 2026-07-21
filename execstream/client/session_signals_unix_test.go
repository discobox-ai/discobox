//go:build !windows

package client

import (
	"os"
	"syscall"
)

// testSuspendSignal and testUnnamedSignal are the signals the fake console
// recognizes and ignores. They are platform-specific: Windows has neither.
var (
	testSuspendSignal os.Signal = syscall.SIGTSTP
	testUnnamedSignal os.Signal = syscall.SIGUSR1
)
