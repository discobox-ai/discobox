package docker

import (
	"testing"
)

func TestDefinitionIncludesSystemdConfig(t *testing.T) {
	def := Definition()
	if def.Name != "Docker" {
		t.Fatalf("name = %q", def.Name)
	}
	keys := map[string]bool{}
	for _, field := range def.ConfigFields {
		keys[field.Key] = true
	}
	for _, key := range []string{"controlPlaneUrl", "image", "systemd", "privileged", "poolSize"} {
		if !keys[key] {
			t.Fatalf("definition missing config key %q", key)
		}
	}
}

func TestNewDriverWithClientDefaultsToSystemdContainer(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	if d.image != defaultImage {
		t.Fatalf("image = %q", d.image)
	}
	if d.agentPort != defaultAgentPort {
		t.Fatalf("agentPort = %d", d.agentPort)
	}
	if d.systemd {
		t.Fatalf("systemd default = true, want false for raw driver config")
	}
	if d.privileged {
		t.Fatalf("privileged default = true, want false when systemd is false")
	}
}

func TestNewDriverWithClientSystemdDefaults(t *testing.T) {
	d := NewDriverWithClient(nil, Config{Systemd: true})
	if !d.systemd {
		t.Fatalf("systemd = false")
	}
	if !d.privileged {
		t.Fatalf("privileged = false, want true for systemd")
	}
	if len(d.command) != 1 || d.command[0] != "/sbin/init" {
		t.Fatalf("command = %#v", d.command)
	}
}

func TestNewDriverWithClientHonorsPrivilegedOverride(t *testing.T) {
	privileged := false
	d := NewDriverWithClient(nil, Config{Systemd: true, Privileged: &privileged})
	if d.privileged {
		t.Fatalf("privileged override ignored")
	}
}
