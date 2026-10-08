package sandboxruntime

import (
	"testing"
	"time"
)

func TestConfiguredSandboxIdleTimeout(t *testing.T) {
	t.Setenv(EnvSandboxIdleTimeout, "")
	if got, err := ConfiguredSandboxIdleTimeout(); err != nil || got != 0 {
		t.Fatalf("unset = %s, %v; want zero so the sandbox-agent default applies", got, err)
	}
	t.Setenv(EnvSandboxIdleTimeout, "2m0s")
	if got, err := ConfiguredSandboxIdleTimeout(); err != nil || got != 2*time.Minute {
		t.Fatalf("2m0s = %s, %v", got, err)
	}
	for _, bad := range []string{"soon", "0s", "-1m"} {
		t.Setenv(EnvSandboxIdleTimeout, bad)
		if _, err := ConfiguredSandboxIdleTimeout(); err == nil {
			t.Fatalf("%q was accepted", bad)
		}
	}
}
