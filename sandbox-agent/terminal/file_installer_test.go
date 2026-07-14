package terminal

import (
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

func int64ptr(v int64) *int64 { return &v }

// The file installer must resolve the same HOME the process env defaults do, so
// harness config files land where the harness will look for them.
func TestFileInstallerResolveHomeMatchesProcessEnv(t *testing.T) {
	user := &execs.User{UID: int64ptr(0)}
	env, err := execs.UserEnvDefaults(user)
	if err != nil {
		t.Fatalf("user env defaults: %v", err)
	}
	procHome := env["HOME"]
	if procHome == "" {
		t.Skip("uid 0 home not resolvable in this environment")
	}

	installerHome, err := FileInstaller{UID: int64ptr(0)}.resolveHome()
	if err != nil {
		t.Fatalf("installer resolveHome: %v", err)
	}
	if installerHome != procHome {
		t.Fatalf("installer home %q != process env home %q", installerHome, procHome)
	}
}

// With no explicit home and no resolvable run user, the installer falls back to
// the harness process's own $HOME rather than erroring (the old behavior).
func TestFileInstallerResolveHomeFallsBackToEnv(t *testing.T) {
	t.Setenv("HOME", "/fallback-home")
	home, err := FileInstaller{}.resolveHome()
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	if home != "/fallback-home" {
		t.Fatalf("home = %q, want /fallback-home", home)
	}
}
