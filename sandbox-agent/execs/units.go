package execs

import (
	"sort"
	"strings"
)

// unitType is the suffix systemd gives every exec unit. Unit names are stored
// and passed around without it, whatever the unit manager — nextUnitGeneration
// parses a bare name, and a stored name with the suffix would make every
// relaunch collide on generation 2 — so SystemdRunner appends it at the D-Bus
// boundary, and unitBaseName strips it off everything coming back, including
// records an earlier runner stored with it.
const unitType = ".service"

// unitBaseName is the name stored on an Exec.
func unitBaseName(name string) string {
	return strings.TrimSuffix(strings.TrimSpace(name), unitType)
}

// unitEnvironment renders the exec's environment as the environment of the
// process a unit runs: systemd's Environment property, or a supervised shim's
// own. Entries are sorted so a unit does not depend on map iteration order.
func unitEnvironment(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for key, value := range env {
		if strings.TrimSpace(key) != "" {
			out = append(out, key+"="+value)
		}
	}
	sort.Strings(out)
	return out
}
