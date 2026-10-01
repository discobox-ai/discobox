//go:build !linux

package execs

import "path/filepath"

// defaultUnitManager is the unit manager a Manager supervises its execs with
// when it was given none. Off Linux there is no systemd, so the agent
// supervises its own shims (ADR 0145 §4), keeping their state beside the
// runtime files they write.
func defaultUnitManager(runtimeDir string) UnitManager {
	return NewSupervisor(filepath.Join(runtimeDir, supervisorDirName))
}
