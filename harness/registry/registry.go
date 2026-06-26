package registry

import (
	"context"
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

func NewInstaller() Installer {
	return Installer{Drivers: DefaultDrivers()}
}

func (i Installer) Install(ctx context.Context, req harness.InstallRequest) error {
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
		if err := driver.Install(ctx, req); err != nil {
			return err
		}
	}
	return nil
}

func DriverForAgent(agent harness.Agent) []harness.Driver {
	id := strings.ToLower(strings.TrimSpace(agent.ID))
	command := strings.ToLower(strings.Join(agent.Command, " "))
	switch {
	case strings.Contains(id, "claude") || strings.Contains(command, "claude"):
		return []harness.Driver{claudecode.Driver{}}
	case strings.Contains(id, "opencode") || strings.Contains(command, "opencode"):
		return []harness.Driver{opencode.Driver{}}
	case strings.Contains(id, "codex") || strings.Contains(command, "codex"):
		return []harness.Driver{codexcli.Driver{}}
	default:
		return DefaultDrivers()
	}
}
