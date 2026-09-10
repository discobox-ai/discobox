//go:build windows

package execs

import "os/exec"

func killOneShot(cmd *exec.Cmd) error { return cmd.Process.Kill() }
