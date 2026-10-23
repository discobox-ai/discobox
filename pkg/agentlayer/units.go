package agentlayer

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

// unitFiles are the agent's own units: what boots a box before any feature
// is provisioned, so they cannot be a feature's. They run the agent from
// AgentPath.
//
//go:embed units/*
var unitFiles embed.FS

// Unit is one of the agent's units: its file, where it is installed, and
// the target that pulls it in, the way its [Install] section says.
type Unit struct {
	Name string
	Path string
	// target pulls the unit in: a link in its .wants directory, or its
	// .requires directory when required. Empty is a unit something else
	// starts.
	target   string
	required bool
}

func (u Unit) linkDir() string {
	kind := ".wants"
	if u.required {
		kind = ".requires"
	}
	return "/etc/systemd/system/" + u.target + kind
}

// Units are every unit the agent boots a box with. A unit file's [Install]
// section and its entry here say the same thing: the entry is what lays the
// link where no systemctl runs, as in a container that has not started.
var Units = []Unit{
	{Name: "discobox-wire.service", Path: "/etc/systemd/system/discobox-wire.service", target: "sysinit.target", required: true},
	{Name: "discobox-sandbox-agent.service", Path: "/etc/systemd/system/discobox-sandbox-agent.service"},
	{Name: "discobox-sandbox-agent.path", Path: "/etc/systemd/system/discobox-sandbox-agent.path", target: "paths.target"},
	{Name: "discobox-proxy-bridge.service", Path: "/etc/systemd/system/discobox-proxy-bridge.service", target: "multi-user.target"},
	{Name: "discobox-trust-ca.service", Path: "/etc/systemd/system/discobox-trust-ca.service", target: "sysinit.target"},
	{Name: "discobox-snapshot.service", Path: "/etc/systemd/system/discobox-snapshot.service", target: "multi-user.target"},
	{Name: "discobox-render-proxy-env.service", Path: "/etc/systemd/system/discobox-render-proxy-env.service", target: "sysinit.target"},
	{Name: "journald-console.conf", Path: "/etc/systemd/journald.conf.d/discobox-console.conf"},
}

// Install lays the units and their enablement under root, as Archive does in
// a container: what a machine's first-boot command does before it starts the
// agent. Reloading systemd is the caller's.
func Install(root string) error {
	for _, unit := range Units {
		content, err := unitFiles.ReadFile("units/" + unit.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(root, unit.Path)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		tmp := target + ".tmp"
		//nolint:gosec // G306: a unit file is read by systemd and every user, as each under /etc/systemd/system is.
		if err := os.WriteFile(tmp, content, 0o644); err != nil {
			return err
		}
		if err := os.Rename(tmp, target); err != nil {
			return err
		}
		if unit.target == "" {
			continue
		}
		dir := filepath.Join(root, unit.linkDir())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		link := filepath.Join(dir, unit.Name)
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.Symlink(unit.Path, link); err != nil {
			return fmt.Errorf("enable %s: %w", unit.Name, err)
		}
	}
	return nil
}
