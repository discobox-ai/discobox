package autostop

import (
	"os"
	"testing"
)

// The power-off cannot be run in a test, but what it runs can be checked to be
// there: a missing path would only show as a sandbox that never stops.
func TestPlatformPowerOffRunsAProgramThatExists(t *testing.T) {
	info, err := os.Stat(shutdownPath)
	if err != nil {
		t.Fatalf("stat %s: %v", shutdownPath, err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("%s is not executable: %v", shutdownPath, info.Mode())
	}
}
