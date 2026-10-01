//go:build windows

package execs

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// The Supervisor's lifetime lock is a flock, which passes to a child with its
// descriptor. A Windows file lock belongs to the handle and process that took
// it and is not inherited, so this half waits on the Windows sandbox work that
// gives the supervisor its own way to hold a shim's lifetime (ADR 0145 §4).
// Until then it builds, and reports every lock as unsupported — which the
// supervisor never reads as a shim that ended.
func lockFile(*os.File, bool) (bool, error) { return false, errors.ErrUnsupported }

func shimSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

func inheritLifetime(*exec.Cmd, *os.File) string { return "" }

func holdLifetime(uintptr) error { return errors.ErrUnsupported }

// terminateProcess has no signal to send a Windows process; ending it is the
// nearest real mechanism.
func terminateProcess(pid int) error { return killProcess(pid) }

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Kill()
}
