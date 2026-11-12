package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// An entry the account added to a directory's ACL while it held it is gone
// once clearACL has run: on macOS a change of owner or mode would have kept
// it, and with it the account's right to add and remove names there.
func TestClearingAnACLRemovesWhatTheAccountAdded(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	dir := filepath.Join(t.TempDir(), "agent-tls")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory with no ACL, as the pool places one, clears too: the drop
	// runs this at every first boot.
	if err := clearACL(dir); err != nil {
		t.Fatalf("clear a directory with no ACL: %v", err)
	}
	if out, err := exec.CommandContext(t.Context(), chmodPath, "+a", current.Username+" allow add_file,delete_child", dir).CombinedOutput(); err != nil { //nolint:gosec // G204: the test account and its own directory.
		t.Fatalf("add an ACL entry: %v: %s", err, out)
	}
	if !strings.Contains(acl(t, dir), "allow add_file") {
		t.Fatalf("no ACL entry to clear:\n%s", acl(t, dir))
	}
	if err := clearACL(dir); err != nil {
		t.Fatal(err)
	}
	if got := acl(t, dir); strings.Contains(got, "allow") {
		t.Fatalf("the ACL survived:\n%s", got)
	}
}

func acl(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/ls", "-led", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ls -led %s: %v: %s", path, err, out)
	}
	return string(out)
}
