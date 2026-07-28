package proxyagent

import (
	"strings"
	"testing"
)

// testProjectID and testPoolID give tests a concrete scope. Every pool-owned
// path is scoped now, so a test cannot address one without naming its pool —
// which is the property being protected.
const (
	testProjectID = "prj-test"
	testPoolID    = "pool-test"
)

// The proxy unit runs with a clean systemd environment and addresses only
// pool-scoped paths, so it cannot start without knowing which pool it serves.
// Leaving these out fails at proxy startup with an error far from the cause —
// which is exactly what happened while this scoping was being introduced.
func TestUnitEnvironmentNamesThePool(t *testing.T) {
	content := unitEnvironment("/host", "unix:///run/discobox/cp.sock", testProjectID, testPoolID)
	for _, want := range []string{
		envProjectID + "=" + testProjectID,
		envPoolID + "=" + testPoolID,
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("unit environment is missing %q; the proxy cannot address its own state:\n%s", want, content)
		}
	}
}

// The host-mount prefix and control-plane URL must survive alongside the new
// scope entries.
func TestUnitEnvironmentKeepsTransportSettings(t *testing.T) {
	content := unitEnvironment("/host", "unix:///run/discobox/cp.sock", testProjectID, testPoolID)
	for _, want := range []string{
		envHostMountPrefix + "=/host",
		envControlPlaneURL + "=unix:///run/discobox/cp.sock",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("unit environment is missing %q:\n%s", want, content)
		}
	}
}
