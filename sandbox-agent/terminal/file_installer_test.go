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

	installerHome, err := FileInstaller{User: user}.resolveHome(env)
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
	home, err := FileInstaller{}.resolveHome(nil)
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	if home != "/fallback-home" {
		t.Fatalf("home = %q, want /fallback-home", home)
	}
}

// A sandbox whose manifest names nobody still runs as somebody, and its harness
// files still have to land somewhere. This is every sandbox the server creates
// for itself — a configure sandbox carries no user at all — and it used to fail
// with "no home directory could be resolved for the run user" while the exec
// starting two lines later found one without trouble.
func TestFileInstallerResolvesHomeForASandboxThatNamesNoUser(t *testing.T) {
	// What the exec layer hands over when nobody is named: no user, and the
	// env the harness will actually run with.
	env := map[string]string{"HOME": "/home/agent"}

	home, err := FileInstaller{}.resolveHome(env)
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	if home != "/home/agent" {
		t.Fatalf("home = %q, want the run env's home", home)
	}
}

// An explicit home in the manifest still wins over everything.
func TestFileInstallerPrefersTheExplicitHome(t *testing.T) {
	home, err := FileInstaller{
		HomeDirectory: "/explicit",
		User:          &execs.User{HomeDirectory: "/from-user"},
	}.resolveHome(map[string]string{"HOME": "/from-env"})
	if err != nil {
		t.Fatalf("resolveHome: %v", err)
	}
	if home != "/explicit" {
		t.Fatalf("home = %q, want the manifest's explicit home", home)
	}
}
