package registry

import (
	"strings"

	"github.com/discobox-ai/discobox/harness"
	claudecode "github.com/discobox-ai/discobox/harness/claude-code"
	codexcli "github.com/discobox-ai/discobox/harness/codex-cli"
	"github.com/discobox-ai/discobox/harness/shell"
)

func DefaultDrivers() []harness.Driver {
	return []harness.Driver{
		claudecode.Driver{},
		codexcli.Driver{},
		// Last because it is the one every sandbox falls back to rather than
		// one anybody picks for its own sake; it is otherwise an ordinary
		// registry harness (ADR 0043).
		shell.Driver{},
	}
}

// Definitions returns the built-in harness-config template for every known
// harness, in default-driver order.
func Definitions() []harness.Definition {
	drivers := DefaultDrivers()
	out := make([]harness.Definition, 0, len(drivers))
	for _, driver := range drivers {
		out = append(out, driver.Definition())
	}
	return out
}

// DriverForHarness selects the hook driver from the harness type baked into the
// image. Falls back to all drivers for old or externally supplied harnesses.
func DriverForHarness(h harness.Harness) []harness.Driver {
	typeID := strings.TrimSpace(h.TypeID)
	for _, driver := range DefaultDrivers() {
		if typeID != "" && typeID == driver.Definition().ID {
			return []harness.Driver{driver}
		}
	}
	return DefaultDrivers()
}
