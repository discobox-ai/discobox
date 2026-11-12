package autostop

import (
	"os"
	"strings"
	"testing"
)

// The power-off cannot be run in a test, but what it runs can be checked to be
// there: a missing path would only show as a sandbox that never stops.
func TestPlatformPowerOffRunsAProgramThatExists(t *testing.T) {
	for _, path := range []string{shutdownPath, sudoPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode()&0o111 == 0 {
			t.Fatalf("%s is not executable: %v", path, info.Mode())
		}
	}
}

// Root halts the guest itself; the sandbox's account, which a dropped agent
// is, asks sudo without waiting on a password.
func TestAnAgentThatIsNotRootHaltsThroughSudo(t *testing.T) {
	if got := strings.Join(shutdownCommand(0), " "); got != "/sbin/shutdown -h now" {
		t.Fatalf("as root: %s", got)
	}
	if got := strings.Join(shutdownCommand(501), " "); got != "/usr/bin/sudo -n /sbin/shutdown -h now" {
		t.Fatalf("as the account: %s", got)
	}
}
