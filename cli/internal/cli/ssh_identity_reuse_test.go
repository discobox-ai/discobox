package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// enrolledUserKey writes a real keypair to <home>/.ssh/<name> and marks it
// enrolled on the fake server, standing in for a key the user added by hand.
func enrolledUserKey(t *testing.T, fake *sshConfigFakeServer, home, name string, enroll bool) string {
	t.Helper()
	path := filepath.Join(home, ".ssh", name)
	line, _, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		t.Fatalf("create user key: %v", err)
	}
	if enroll {
		fake.keys = append(fake.keys, map[string]any{
			"id": "sshkey_user", "projectId": "project-1", "publicKey": line,
			"fingerprint": mustFingerprint(t, line),
			"createdAt":   "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
		})
	}
	return path
}

// TestSSHConfigReusesAnAlreadyEnrolledUserKey is the rule: a key that is both
// enrolled and usable is used as-is. Enrolling another would leave the project
// holding two keys for the same person on the same machine, and revoking either
// would then revoke nothing.
func TestSSHConfigReusesAnAlreadyEnrolledUserKey(t *testing.T) {
	fake := writeFakeServer()
	home := t.TempDir()
	state := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)
	keyPath := enrolledUserKey(t, fake, home, "id_ed25519", true)

	server := fake.start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	if len(fake.enrolled) != 0 {
		t.Fatalf("enrolled %d new keys when one was already available: %v", len(fake.enrolled), fake.enrolled)
	}
	if !strings.Contains(out.String(), "IdentityFile "+keyPath+"\n") {
		t.Fatalf("config does not point at the already-enrolled key %s:\n%s", keyPath, out.String())
	}
	managed := filepath.Join(state, "discobox", "cli", "ssh", "id_ed25519")
	if fileExists(managed) {
		t.Fatal("a managed key was generated even though an enrolled key was available")
	}
	if !strings.Contains(errOut.String(), "already enrolled") {
		t.Fatalf("reuse should be reported, stderr: %q", errOut.String())
	}
}

// TestSSHConfigDoesNotAdoptAnUnenrolledUserKey: a key the user never enrolled
// is not evidence they want it used for this, so generate a scoped one instead
// of enrolling their personal key on their behalf.
func TestSSHConfigDoesNotAdoptAnUnenrolledUserKey(t *testing.T) {
	fake := writeFakeServer()
	home := t.TempDir()
	state := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)
	keyPath := enrolledUserKey(t, fake, home, "id_ed25519", false)

	server := fake.start(t)
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	if strings.Contains(out.String(), keyPath) {
		t.Fatalf("adopted the user's unenrolled key:\n%s", out.String())
	}
	managed := filepath.Join(state, "discobox", "cli", "ssh", "id_ed25519")
	if !fileExists(managed) {
		t.Fatal("no managed key was generated")
	}
	if len(fake.enrolled) != 1 {
		t.Fatalf("enrolled %d keys, want the generated one", len(fake.enrolled))
	}
}

// TestSSHConfigIgnoresAnEnrolledKeyWithNoPrivateHalf: an enrolled fingerprint
// whose private key is not on this machine cannot authenticate from here, so
// naming it as IdentityFile would produce a config that simply fails.
func TestSSHConfigIgnoresAnEnrolledKeyWithNoPrivateHalf(t *testing.T) {
	fake := writeFakeServer()
	home := t.TempDir()
	state := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)
	keyPath := enrolledUserKey(t, fake, home, "id_ed25519", true)
	// Keep the .pub, take away the private half — a key copied from elsewhere.
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove private key: %v", err)
	}

	server := fake.start(t)
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	if strings.Contains(out.String(), "IdentityFile "+keyPath+"\n") {
		t.Fatalf("named a key whose private half is missing:\n%s", out.String())
	}
	if !fileExists(filepath.Join(state, "discobox", "cli", "ssh", "id_ed25519")) {
		t.Fatal("no usable key was available, so one should have been generated")
	}
}

// TestSSHConfigPrefersTheManagedKeyOnceItExists keeps the answer stable across
// runs: having generated a key, later runs must not switch to a different one.
func TestSSHConfigPrefersTheManagedKeyOnceItExists(t *testing.T) {
	fake := writeFakeServer()
	home := t.TempDir()
	state := t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)
	enrolledUserKey(t, fake, home, "id_ed25519", true)

	managed := filepath.Join(state, "discobox", "cli", "ssh", "id_ed25519")
	managedLine, _, err := loadOrCreateSSHIdentity(managed)
	if err != nil {
		t.Fatalf("create managed key: %v", err)
	}
	fake.keys = append(fake.keys, map[string]any{
		"id": "sshkey_managed", "projectId": "project-1", "publicKey": managedLine,
		"fingerprint": mustFingerprint(t, managedLine),
		"createdAt":   "2026-01-01T00:00:00Z", "updatedAt": "2026-01-01T00:00:00Z",
	})

	server := fake.start(t)
	cmd := NewRootCommand()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(new(strings.Builder))
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "box", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}

	if !strings.Contains(out.String(), "IdentityFile "+managed+"\n") {
		t.Fatalf("config should keep using the managed key:\n%s", out.String())
	}
	if len(fake.enrolled) != 0 {
		t.Fatalf("re-enrolled an already-enrolled key: %v", fake.enrolled)
	}
}
