package registry

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/obot-platform/discobox/harness"
	claudecode "github.com/obot-platform/discobox/harness/claude-code"
	codexcli "github.com/obot-platform/discobox/harness/codex-cli"
	"github.com/obot-platform/discobox/harness/opencode"
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
		opencode.Driver{},
	}
}

// Definitions returns the built-in agent-config template for every known
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

// DriverForAgent selects the harness whose coding-agent CLI a terminal runs,
// identified by the agent's run binary (argv[0]) matched exactly against each
// driver's own definition. The agent's config ID is a random, project-scoped
// identifier, not a harness identifier, so it is not used here. Falls back to
// all drivers when the binary is unknown.
func DriverForAgent(agent harness.Agent) []harness.Driver {
	binary := commandBinary(agent.Command)
	if binary == "" {
		return DefaultDrivers()
	}
	for _, driver := range DefaultDrivers() {
		if binary == commandBinary(driver.Definition().RunCommand) {
			return []harness.Driver{driver}
		}
	}
	return DefaultDrivers()
}

// commandBinary returns the lowercase base name of a command's executable
// (argv[0]), which identifies the coding-agent CLI a terminal runs.
func commandBinary(command []string) string {
	if len(command) == 0 {
		return ""
	}
	first := strings.TrimSpace(command[0])
	if first == "" {
		return ""
	}
	return strings.ToLower(filepath.Base(first))
}
