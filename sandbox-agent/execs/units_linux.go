package execs

// defaultUnitManager is the unit manager a Manager supervises its execs with
// when it was given none: systemd, which a Linux sandbox's PID 1 is.
func defaultUnitManager(string) UnitManager {
	return NewSystemdRunner()
}
