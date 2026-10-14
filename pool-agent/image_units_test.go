package poolagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unitLines are a unit file's settings: its lines without comments and blanks.
func unitLines(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("image", "systemd", filepath.FromSlash(name)))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	return lines
}

func requireUnitLine(t *testing.T, name, want string) {
	t.Helper()
	for _, line := range unitLines(t, name) {
		if line == want {
			return
		}
	}
	t.Errorf("%s has no line %q", name, want)
}

// The machine's units and BootstrapFile name one path: the path unit waits for
// the file a provider places, and the agent's unit loads that file.
func TestMachineUnitsWaitForAndLoadTheBootstrapFile(t *testing.T) {
	requireUnitLine(t, "discobox-pool-agent.path", "PathExists="+BootstrapFile)
	requireUnitLine(t, "discobox-pool-agent.path", "Unit=discobox-pool-agent.service")
	// No leading "-": an agent started with no bootstrap would register as
	// nothing, so a missing file fails the start instead.
	requireUnitLine(t, "discobox-pool-agent.service", "EnvironmentFile="+BootstrapFile)
}

// In a machine the proxy starts only after the agent has written what it
// reads, which the agent says by notifying (agentsystemd.NotifyReady), and it
// is started by the agent's unit rather than at boot, when there is no pool
// yet to be the proxy of.
func TestMachineProxyIsOrderedAfterTheAgentIsReady(t *testing.T) {
	requireUnitLine(t, "discobox-pool-agent.service", "Type=notify")
	requireUnitLine(t, "discobox-proxy.service.d/after-agent.conf", "After=discobox-pool-agent.service")
	// Requisite: the proxy does not start when the agent's start failed.
	requireUnitLine(t, "discobox-proxy.service.d/after-agent.conf", "Requisite=discobox-pool-agent.service")
	requireUnitLine(t, "discobox-proxy.service.d/after-agent.conf", "PartOf=discobox-pool-agent.service")
	// Requires= or BindsTo= would have the proxy's start ask for the agent's,
	// and the agent's unit starts the proxy: an agent failing before it is
	// ready is then restarted at once, around its RestartSec.
	for _, line := range unitLines(t, "discobox-proxy.service.d/after-agent.conf") {
		if strings.HasPrefix(line, "Requires=") || strings.HasPrefix(line, "BindsTo=") {
			t.Errorf("the proxy's drop-in has %q, which starts the agent from the proxy", line)
		}
	}
	wanted := false
	for _, line := range unitLines(t, "discobox-pool-agent.service") {
		if rest, ok := strings.CutPrefix(line, "Wants="); ok {
			wanted = wanted || strings.Contains(" "+rest+" ", " discobox-proxy.service ")
		}
	}
	if !wanted {
		t.Error("discobox-pool-agent.service does not want discobox-proxy.service, so nothing starts the proxy")
	}
}

// The agent's unit has no [Install] section: only its bootstrap starts it.
func TestMachineAgentUnitIsStartedOnlyByItsBootstrap(t *testing.T) {
	for _, line := range unitLines(t, "discobox-pool-agent.service") {
		if line == "[Install]" {
			t.Error("discobox-pool-agent.service can be enabled, and would start with no bootstrap")
		}
	}
	requireUnitLine(t, "discobox-pool-agent.path", "WantedBy=paths.target")
}
