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
	if !shouldAutoLaunchServer(false) {
		t.Fatal("release build should auto-launch the server")
	}
	if shouldAutoLaunchServer(true) {
		t.Fatal("--no-start should disable server auto-launch")
	}

	serverAutoLaunch = "false"
	if shouldAutoLaunchServer(false) {
		t.Fatal("development build should not auto-launch the server")
	}
}
