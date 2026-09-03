package cli

import "testing"

func TestServerAutoLaunchDisabledByDefault(t *testing.T) {
	if serverAutoLaunch != "false" {
		t.Fatalf("serverAutoLaunch = %q, want disabled default", serverAutoLaunch)
	}
}

func TestShouldAutoLaunchServer(t *testing.T) {
	original := serverAutoLaunch
	t.Cleanup(func() {
		serverAutoLaunch = original
	})

	serverAutoLaunch = "true"
	if !shouldAutoLaunchServer(autoStartServerAuto) {
		t.Fatal("release build should auto-launch the server")
	}
	if shouldAutoLaunchServer(autoStartServerFalse) {
		t.Fatal("--auto-start-server=false should disable server auto-launch")
	}

	serverAutoLaunch = "false"
	if shouldAutoLaunchServer(autoStartServerAuto) {
		t.Fatal("development build should not auto-launch the server")
	}
	// --auto-start-server=true overrides a development build the other way,
	// which is the reason the flag is three-way rather than a plain bool: a
	// development build could otherwise never be told to launch one without
	// DISCOBOX_SERVER_AUTOLAUNCH.
	if !shouldAutoLaunchServer(autoStartServerTrue) {
		t.Fatal("--auto-start-server=true should force a development build to auto-launch the server")
	}
}
