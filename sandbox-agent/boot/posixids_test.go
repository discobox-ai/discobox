package boot

import (
	"runtime"
	"testing"

	"github.com/discobox-ai/discobox/sandboxuser"
)

// skipWithoutPOSIXIDs skips a test that describes a Linux sandbox's identity:
// uids, gids, groups and a passwd database. Elsewhere the exec manager resolves
// the sandbox's one account instead (ADR 0145 §5), which runuser's own tests
// pin on every platform.
func skipWithoutPOSIXIDs(t *testing.T) {
	t.Helper()
	if !sandboxuser.HasPOSIXIDs(runtime.GOOS) {
		t.Skipf("a %s sandbox has one account and no POSIX ids", runtime.GOOS)
	}
}
