package registry

import (
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
