package registry

import (
	"context"
	"strings"

	"github.com/obot-platform/discobox/harness"
	claudecode "github.com/obot-platform/discobox/harness/claude-code"
	codexcli "github.com/obot-platform/discobox/harness/codex-cli"
	"github.com/obot-platform/discobox/harness/shell"
)

type Installer struct {
	Drivers          []harness.Driver
	PublisherCommand string
	ManagedRoot      string
}

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

func NewInstaller() Installer {
	return Installer{Drivers: DefaultDrivers()}
}

func (i Installer) InstallHooks(ctx context.Context, req harness.HookInstallRequest) error {
	if req.PublisherCommand == "" {
		req.PublisherCommand = i.PublisherCommand
	}
	if req.ManagedRoot == "" {
		req.ManagedRoot = i.ManagedRoot
	}
	for _, driver := range i.Drivers {
		if driver == nil {
			continue
		}
		if err := driver.InstallHooks(ctx, req); err != nil {
			return err
		}
	}
	return nil
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
