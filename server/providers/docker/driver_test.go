package docker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

func TestDefinitionIncludesSystemdConfig(t *testing.T) {
	def := Definition()
	if def.Name != "Docker" {
		t.Fatalf("name = %q", def.Name)
	}
	keys := map[string]bool{}
	var controlPlaneRequired, controlPlaneAdvanced bool
	for _, field := range def.ConfigFields {
		keys[field.Key] = true
		if field.Key == "controlPlaneUrl" {
			controlPlaneRequired = field.Required
			controlPlaneAdvanced = field.Advanced
		}
	}
	for _, key := range []string{"controlPlaneUrl", "image", "systemd", "privileged"} {
		if !keys[key] {
			t.Fatalf("definition missing config key %q", key)
		}
	}
	if controlPlaneRequired {
		t.Fatalf("controlPlaneUrl is required, want optional")
	}
	if !controlPlaneAdvanced {
		t.Fatalf("controlPlaneUrl advanced = false, want true")
	}
}

func TestEngineConfigDerivesControlPlaneURLWithHostGateway(t *testing.T) {
	t.Setenv("PORT", "9090")
	cfg := engineConfig(Config{})

	if cfg.ControlPlaneURL != "http://host.docker.internal:9090" {
		t.Fatalf("control plane url = %q", cfg.ControlPlaneURL)
	}
	if len(cfg.ExtraHosts) != 1 || cfg.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Fatalf("extra hosts = %#v, want host gateway mapping", cfg.ExtraHosts)
	}
}

func TestEngineConfigPreservesConfiguredControlPlaneURL(t *testing.T) {
	cfg := engineConfig(Config{ControlPlaneURL: "http://control.example"})

	if cfg.ControlPlaneURL != "http://control.example" {
		t.Fatalf("control plane url = %q", cfg.ControlPlaneURL)
	}
	if len(cfg.ExtraHosts) != 0 {
		t.Fatalf("extra hosts = %#v, want none for configured URL", cfg.ExtraHosts)
	}
}

func TestEngineConfigDefaultsSystemdOn(t *testing.T) {
	cfg := engineConfig(Config{})
	if !cfg.Systemd {
		t.Fatalf("systemd = false, want default true")
	}
	off := false
	cfg = engineConfig(Config{Systemd: &off})
	if cfg.Systemd {
		t.Fatalf("systemd = true, want configured false")
	}
}

func TestEngineConfigDoesNotPublishAgentPortPublicly(t *testing.T) {
	cfg := engineConfig(Config{})
	if cfg.PublicAgentPort {
		t.Fatalf("public harness port = true, want loopback publishing for local workers")
	}
}

// TestProviderConfigFieldsAffectWorkerConfigRevision ensures every persisted
// provider config field either changes the engine config revision (so workers
// get recreated) or is explicitly excluded.
func TestProviderConfigFieldsAffectWorkerConfigRevision(t *testing.T) {
	falseValue := false
	base := Config{ControlPlaneURL: "http://control.example", Image: "worker-image"}
	excludedFields := map[string]string{
		"host":              "Docker daemon connection does not change worker container config",
		"poolSize":          "worker pool sizing does not change worker container config",
		"minWorkers":        "worker pool sizing does not change worker container config",
		"maxWorkers":        "worker pool sizing does not change worker container config",
		"minHealthyWorkers": "worker pool sizing does not change worker container config",
	}
	mutators := map[string]func(*Config){
		"controlPlaneUrl":  func(cfg *Config) { cfg.ControlPlaneURL = "http://other-control.example" },
		"image":            func(cfg *Config) { cfg.Image = "other-worker-image" },
		"network":          func(cfg *Config) { cfg.Network = "discobox-net" },
		"agentPort":        func(cfg *Config) { cfg.AgentPort = 3902 },
		"systemd":          func(cfg *Config) { cfg.Systemd = &falseValue },
		"privileged":       func(cfg *Config) { cfg.Privileged = &falseValue },
		"cgroupNsMode":     func(cfg *Config) { cfg.CgroupNSMode = "host" },
		"command":          func(cfg *Config) { cfg.Command = []string{"/bin/discobox-worker-agent", "--debug"} },
		"bindDockerSocket": func(cfg *Config) { cfg.DockerSocket = "/run/user/1000/docker.sock" },
		"hostMounts":       func(cfg *Config) { cfg.HostMounts = []HostMount{{Source: "/home", ReadOnly: true}} },
	}

	for _, field := range configJSONFields(t, reflect.TypeOf(Config{})) {
		if _, excluded := excludedFields[field]; excluded {
			continue
		}
		mutate, ok := mutators[field]
		if !ok {
			t.Fatalf("provider config field %q must affect worker config revision or be explicitly excluded", field)
		}

		cfg := base
		mutate(&cfg)
		if configRevisionFor(t, base) == configRevisionFor(t, cfg) {
			t.Fatalf("provider config field %q did not change worker config revision", field)
		}
	}
}

func configRevisionFor(t *testing.T, cfg Config) string {
	t.Helper()
	engine, err := dockerworker.New(engineConfig(cfg), &LocalDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine.ConfigRevision()
}

func configJSONFields(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		t.Fatalf("config type kind = %s, want struct", typ.Kind())
	}
	var fields []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.Anonymous {
			fields = append(fields, configJSONFields(t, field.Type)...)
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields = append(fields, name)
	}
	return fields
}
