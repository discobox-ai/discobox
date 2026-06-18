//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !windows

package agent

import (
	"fmt"
	"os"
	"runtime"
)

func lockSessionStoreFile(_ *os.File) error {
	return fmt.Errorf("session store locking is unsupported on %s", runtime.GOOS)
}

func unlockSessionStoreFile(_ *os.File) error {
	return nil
}
