package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
	"github.com/discobox-ai/discobox/proxy"
)

// The pool bundle's tree loads (every provision.yaml valid, every feature's
// requirement in it), with the binaries the build copies beside the
// features in place, and its units read the files a maker places at the
// paths this package names.
func TestThePoolBundleLoads(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, provisionspec.FeaturesDir), os.DirFS("features")); err != nil {
		t.Fatal(err)
	}
	// What the Dockerfile's bundle stage copies in from the build.
	for _, binary := range []string{
		"pool-agent/discobox-pool-agent",
		"pool-builder/bin/discobox-pool-runc",
		"pool-builder/bin/buildkitd",
		"pool-builder/bin/registry",
	} {
		path := filepath.Join(root, provisionspec.FeaturesDir, binary)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	features, err := provisionspec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, feature := range features {
		names = append(names, feature.Name)
	}
	if got := strings.Join(names, " "); got != "pool-agent pool-builder pool-builder-units" {
		t.Fatalf("features = %s", got)
	}

	for _, feature := range features {
		var dependency string
		switch feature.Name {
		case "pool-agent":
			dependency = "pool-builder-units"
		case "pool-builder":
			dependency = "pool-agent"
		default:
			continue
		}
		if !slices.Contains(feature.Spec.Requires, dependency) {
			t.Errorf("%s must wait for %s", feature.Name, dependency)
		}
	}
	unit, err := os.ReadFile(filepath.Join("features", "pool-agent", "systemd", "discobox-pool-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DISCOBOX_POOL_BOOTSTRAP_FILE=" + BootstrapFile, "EnvironmentFile=-" + EnvironmentFile, "Type=notify"} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("the agent's unit does not say %s", want)
		}
	}
}

// Starting the agent as a systemd service must not lose the proxy injected
// into PID 1 by an outer sandbox. Keep the unit's opt-in aligned with the
// upstream transports, including both cases and the trust needed for MITM.
func TestPoolAgentInheritsUpstreamEnvironment(t *testing.T) {
	unit, err := os.ReadFile(filepath.Join("features", "pool-agent", "systemd", "discobox-pool-agent.service"))
	if err != nil {
		t.Fatal(err)
	}
	passed := map[string]bool{}
	for line := range strings.SplitSeq(string(unit), "\n") {
		if value, ok := strings.CutPrefix(line, "PassEnvironment="); ok {
			for _, name := range strings.Fields(value) {
				passed[name] = true
			}
		}
	}
	want := append([]string{"NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}, proxy.UpstreamProxyEnvVars...)
	for _, name := range want {
		if !passed[name] {
			t.Errorf("pool agent loses inherited %s at the systemd boundary", name)
		}
		delete(passed, name)
	}
	if len(passed) != 0 {
		t.Errorf("pool agent inherits unrelated environment variables: %v", passed)
	}
}

// The units a maker restarts on a new bootstrap are the pool-agent feature's
// own, and the agent's is the one that reads the bootstrap.
func TestBootstrapUnitsAreTheFeaturesUnits(t *testing.T) {
	for _, unit := range BootstrapUnits {
		data, err := os.ReadFile(filepath.Join("features", "pool-agent", "systemd", unit))
		if err != nil {
			t.Fatalf("bootstrap unit %s is not the pool-agent feature's: %v", unit, err)
		}
		if unit == "discobox-pool-agent.service" && !strings.Contains(string(data), BootstrapFile) {
			t.Fatalf("%s does not read %s", unit, BootstrapFile)
		}
	}
}

func TestAgentRestartDoesNotStopBuildMediator(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("features", "pool-builder-units", "systemd", "discobox-buildkit-mediator.service"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "Requires" && key != "BindsTo" && key != "PartOf") {
			continue
		}
		for _, unit := range strings.Fields(value) {
			if unit == "discobox-pool-agent.service" {
				t.Errorf("mediator %s=%s propagates agent stop into its build drain, blocking agent/proxy restart", key, unit)
			}
		}
	}
}

// Every path from the agent into the builder must leave builds running.
func TestAgentRestartLeavesBuilderRunning(t *testing.T) {
	for _, name := range []string{"discobox-buildkit-mediator", "discobox-buildkitd", "discobox-registry"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("features", "pool-builder-units", "systemd", name+".service"))
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(string(data), "\n") {
				key, value, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				for _, unit := range strings.Fields(value) {
					if unit != "discobox-pool-agent.service" {
						continue
					}
					if key == "Requires" || key == "BindsTo" || key == "PartOf" {
						t.Errorf("%s=%s propagates agent shutdown", key, unit)
					}
				}
			}
			for _, key := range []string{"Wants", "After"} {
				found := false
				for line := range strings.SplitSeq(string(data), "\n") {
					if value, ok := strings.CutPrefix(line, key+"="); ok {
						for _, unit := range strings.Fields(value) {
							found = found || unit == "discobox-pool-agent.service"
						}
					}
				}
				if !found {
					t.Errorf("missing %s dependency on agent", key)
				}
			}
		})
	}
}

// Every unit that carries a sandbox's traffic loads the in-machine rule set
// before it starts, and does not start without it: a reboot clears the
// kernel's tables, and these units do not wait on the agent's start (ADR
// 26-11-11-080 §9). The agent's own unit is the one exempt.
func TestSandboxTrafficUnitsLoadTheInMachineFence(t *testing.T) {
	const pre = "ExecStartPre=/usr/local/bin/discobox-pool-agent in-machine-fence"
	for _, unit := range []string{
		filepath.Join("features", "pool-agent", "systemd", "discobox-proxy.service"),
		filepath.Join("features", "pool-builder-units", "systemd", "discobox-buildkitd.service"),
		filepath.Join("features", "pool-builder-units", "systemd", "discobox-buildkit-mediator.service"),
		filepath.Join("features", "pool-builder-units", "systemd", "discobox-registry.service"),
	} {
		data, err := os.ReadFile(unit)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{pre, "Environment=DISCOBOX_POOL_BOOTSTRAP_FILE=" + BootstrapFile, "StartLimitIntervalSec=0"} {
			if !strings.Contains(string(data), "\n"+want+"\n") {
				t.Errorf("%s lacks %q: it loads the in-machine fence before it starts, from the bootstrap the agent reads, and is retried for good", unit, want)
			}
		}
	}
	matches, err := filepath.Glob(filepath.Join("features", "*", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 5 {
		t.Errorf("units = %v: a new one that carries a sandbox's traffic loads the in-machine fence too", matches)
	}
}
