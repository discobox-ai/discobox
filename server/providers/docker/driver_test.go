package docker

import (
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

func TestDefinitionIncludesWorkerConfig(t *testing.T) {
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
	for _, key := range []string{"controlPlaneUrl", "image", "privileged"} {
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

const (
	testLocalDaemon  = "unix:///var/run/docker.sock"
	testRemoteDaemon = "tcp://docker.example:2376"
	testUnixListen   = "unix:///run/user/1000/discobox/server.sock"
	testHTTPListen   = "http://0.0.0.0:9090"
)

func engineConfigFor(t *testing.T, cfg Config, listen []string, daemonHost string) dockerworker.Config {
	t.Helper()
	engineCfg, err := engineConfig(cfg, listen, daemonHost)
	if err != nil {
		t.Fatalf("engineConfig() error = %v", err)
	}
	return engineCfg
}

// A local daemon reaches the control plane over the socket it is already
// listening on, so no HTTP listener is needed for a Docker pool to register.
func TestEngineConfigUsesControlPlaneSocketForLocalDaemon(t *testing.T) {
	cfg := engineConfigFor(t, Config{}, []string{testUnixListen}, testLocalDaemon)

	if cfg.ControlPlaneURL != "unix:///run/user/1000/discobox/server.sock" {
		t.Fatalf("control plane url = %q, want the control plane socket", cfg.ControlPlaneURL)
	}
	if cfg.RelaySocketDir != "/run/user/1000/discobox" {
		t.Fatalf("relay socket dir = %q, want the socket's directory bound into the pool", cfg.RelaySocketDir)
	}
	if len(cfg.ExtraHosts) != 0 {
		t.Fatalf("extra hosts = %#v, want none for a socket transport", cfg.ExtraHosts)
	}
}

// The socket is preferred even when HTTP is also listening: it is the direct
// path and needs no gateway alias.
func TestEngineConfigPrefersSocketOverHTTPForLocalDaemon(t *testing.T) {
	cfg := engineConfigFor(t, Config{}, []string{testUnixListen, testHTTPListen}, testLocalDaemon)

	if !strings.HasPrefix(cfg.ControlPlaneURL, "unix://") {
		t.Fatalf("control plane url = %q, want the socket transport", cfg.ControlPlaneURL)
	}
}

// A remote daemon cannot see the socket, so it takes the HTTP listener, mapped
// to the address a container resolves.
func TestEngineConfigUsesHTTPListenerForRemoteDaemon(t *testing.T) {
	cfg := engineConfigFor(t, Config{}, []string{testUnixListen, testHTTPListen}, testRemoteDaemon)

	if cfg.ControlPlaneURL != "http://host.docker.internal:9090" {
		t.Fatalf("control plane url = %q", cfg.ControlPlaneURL)
	}
	if cfg.RelaySocketDir != "" {
		t.Fatalf("relay socket dir = %q, want none for an HTTP transport", cfg.RelaySocketDir)
	}
	if len(cfg.ExtraHosts) != 1 || cfg.ExtraHosts[0] != "host.docker.internal:host-gateway" {
		t.Fatalf("extra hosts = %#v, want host gateway mapping", cfg.ExtraHosts)
	}
}

// A pool that could never register is a configuration error worth naming, not a
// silently broken pool.
func TestEngineConfigFailsWhenNoEndpointIsReachable(t *testing.T) {
	_, err := engineConfig(Config{}, []string{testUnixListen}, testRemoteDaemon)
	if err == nil {
		t.Fatal("engineConfig() succeeded with no endpoint the daemon can reach")
	}
	if !strings.Contains(err.Error(), "DISCOBOX_SERVER_LISTEN") {
		t.Fatalf("error = %v, want it to name how to configure a reachable endpoint", err)
	}
}

func TestEngineConfigPreservesConfiguredControlPlaneURL(t *testing.T) {
	cfg := engineConfigFor(t, Config{ControlPlaneURL: "http://control.example"}, []string{testUnixListen}, testLocalDaemon)

	if cfg.ControlPlaneURL != "http://control.example" {
		t.Fatalf("control plane url = %q", cfg.ControlPlaneURL)
	}
	if cfg.RelaySocketDir != "" {
		t.Fatalf("relay socket dir = %q, want none for a configured URL", cfg.RelaySocketDir)
	}
	if len(cfg.ExtraHosts) != 0 {
		t.Fatalf("extra hosts = %#v, want none for configured URL", cfg.ExtraHosts)
	}
}

func TestEngineConfigDoesNotPublishAgentPortPublicly(t *testing.T) {
	cfg := engineConfigFor(t, Config{}, []string{testUnixListen}, testLocalDaemon)
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
		"privileged":       func(cfg *Config) { cfg.Privileged = &falseValue },
		"cgroupNsMode":     func(cfg *Config) { cfg.CgroupNSMode = "host" },
		"command":          func(cfg *Config) { cfg.Command = []string{"/bin/discobox-pool-agent", "--debug"} },
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
	engine, err := dockerworker.New(engineConfigFor(t, cfg, []string{testUnixListen}, testLocalDaemon), &LocalDriver{})
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
