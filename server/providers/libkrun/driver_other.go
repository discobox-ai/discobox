//go:build !linux

package libkrun

import "syscall"

// launcherSysProcAttr has no parent-death signal to arm off Linux, where no
// launcher is ever started: the driver refuses to construct at all
// (krunvm.Supported). It exists so the driver and its tests compile on every
// platform, as vz's and wslc's do.
func launcherSysProcAttr() *syscall.SysProcAttr { return nil }
