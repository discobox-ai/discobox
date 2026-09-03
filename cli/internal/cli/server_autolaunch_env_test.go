package cli

import "testing"

// The capability is otherwise a property of how the binary was linked, which
// puts the one thing worth testing — does autolaunch work — behind cutting a
// release build.
func TestAutoLaunchEnvEnablesADevelopmentBuild(t *testing.T) {
	t.Setenv(AutoLaunchEnv, "1")
	if !autoLaunchConfigured() {
		t.Fatal("a development build did not honor the environment override")
	}
	if !shouldAutoLaunchServer(autoStartServerAuto) {
		t.Fatal("shouldAutoLaunchServer disagreed with it")
	}
}

// And in the other direction, so a release binary can be made to behave like a
// development one.
func TestAutoLaunchEnvDisablesAReleaseBuild(t *testing.T) {
	previous := serverAutoLaunch
	serverAutoLaunch = "true"
	t.Cleanup(func() { serverAutoLaunch = previous })

	t.Setenv(AutoLaunchEnv, "0")
	if autoLaunchConfigured() {
		t.Fatal("a release build ignored the environment override")
	}
}

// An explicit --auto-start-server is the last word: it is what somebody
// types, and neither the build nor the environment outranks that.
func TestExplicitAutoStartServerBeatsEverything(t *testing.T) {
	t.Setenv(AutoLaunchEnv, "1")
	if shouldAutoLaunchServer(autoStartServerFalse) {
		t.Fatal("--auto-start-server=false was overruled")
	}

	t.Setenv(AutoLaunchEnv, "0")
	if !shouldAutoLaunchServer(autoStartServerTrue) {
		t.Fatal("--auto-start-server=true was overruled")
	}
}

// Unset leaves the build's own answer standing, which for a development build
// is off — the default that keeps a forked server from racing `task dev`.
func TestAutoLaunchDefaultsToTheBuild(t *testing.T) {
	t.Setenv(AutoLaunchEnv, "")
	if autoLaunchConfigured() {
		t.Fatal("a development build autolaunched with nothing asking it to")
	}
}

// A typo decides nothing rather than failing the command: this gates a
// convenience, and refusing to run over it would be the worse answer.
func TestAutoLaunchIgnoresAnUnparseableValue(t *testing.T) {
	t.Setenv(AutoLaunchEnv, "yes-please")
	if autoLaunchConfigured() {
		t.Fatal("an unparseable value was treated as enabling")
	}
}
