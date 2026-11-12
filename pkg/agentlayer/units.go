package agentlayer

import (
	"embed"
	"slices"
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

// Link is where the link that pulls the unit in is, and false for a unit
// something else starts.
func (u Unit) Link() (string, bool) {
	if u.target == "" {
		return "", false
	}
	return u.linkDir() + "/" + u.Name, true
}

// AgentStartUnit is the unit that starts the agent once its bootstrap is in
// place.
const AgentStartUnit = "discobox-sandbox-agent.path"

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
	{Name: "discobox-docker-configuration.service", Path: "/etc/systemd/system/discobox-docker-configuration.service"},
	{Name: "docker-configuration.conf", Path: "/etc/systemd/system/docker.service.d/discobox-configuration.conf"},
	{Name: "docker-configuration.conf", Path: "/etc/systemd/system/containerd.service.d/discobox-configuration.conf"},
	{Name: "discobox-wire.service", Path: "/etc/systemd/system/discobox-wire.service", target: "sysinit.target", required: true},
	{Name: "discobox-sandbox-agent.service", Path: "/etc/systemd/system/discobox-sandbox-agent.service"},
	{Name: "discobox-sandbox-agent.path", Path: "/etc/systemd/system/discobox-sandbox-agent.path", target: "paths.target"},
	{Name: "discobox-proxy-bridge.service", Path: "/etc/systemd/system/discobox-proxy-bridge.service", target: "multi-user.target"},
	{Name: "discobox-trust-ca.service", Path: "/etc/systemd/system/discobox-trust-ca.service", target: "sysinit.target"},
	{Name: "discobox-render-proxy-env.service", Path: "/etc/systemd/system/discobox-render-proxy-env.service", target: "sysinit.target"},
	{Name: "journald-console.conf", Path: "/etc/systemd/journald.conf.d/discobox-console.conf"},
}

// MachineUnits are what a machine sandbox boots with: Units, and the drop-in
// that runs discobox-wire.service where its pool placed the bootstrap rather
// than where a container's config volume is, powering the machine off when
// the wiring fails (ADR 26-11-02-023 §3).
var MachineUnits = append(slices.Clip(Units),
	Unit{Name: "discobox-wire-machine.conf", Path: "/etc/systemd/system/discobox-wire.service.d/discobox-machine.conf"},
)
