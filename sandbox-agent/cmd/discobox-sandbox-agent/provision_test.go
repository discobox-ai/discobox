package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// identity makes the box's key the first time it is asked, and answers its
// public half, the same one every time; the private half is the box's file
// alone (ADR 26-10-24-953 §4).
func TestIdentityAnswersTheSameKeyMadeOnDemand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity", "agent.key")
	var first, second bytes.Buffer
	if code := provisionIdentity(path, &first); code != 0 {
		t.Fatalf("identity exited %d", code)
	}
	if code := provisionIdentity(path, &second); code != 0 {
		t.Fatalf("identity again exited %d", code)
	}
	if first.String() != second.String() || strings.TrimSpace(first.String()) == "" {
		t.Fatalf("identity answered %q then %q", first.String(), second.String())
	}
	key, err := poolauth.LoadKey(path)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := poolauth.PublicKeyOf(key)
	if strings.TrimSpace(first.String()) != public {
		t.Fatalf("identity answered %q, not the key's public half %q", first.String(), public)
	}
	if strings.Contains(first.String(), string(key)) {
		t.Fatal("identity printed the private key")
	}
	if err := os.WriteFile(path, []byte("damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := provisionIdentity(path, &bytes.Buffer{}); code == 0 {
		t.Fatal("identity replaced a damaged key")
	}
}

// apply runs straight after its machine's start, where systemd has made its
// directory under /run but does not answer on its bus yet: it waits for the
// bus, rather than failing the start on the first systemctl that cannot
// reach it.
func TestApplyWaitsForSystemdsBus(t *testing.T) {
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("apply starts nothing where systemd is not running")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	// A systemd whose bus answers from its third call on.
	script := `#!/bin/sh
echo "$*" >> ` + calls + `
if [ "$(wc -l < ` + calls + `)" -le 2 ]; then
	[ "$1" = is-system-running ] && echo offline
	echo "Failed to connect to bus: No such file or directory" >&2
	exit 1
fi
[ "$1" = is-system-running ] && echo running
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0o755); err != nil { //nolint:gosec // G306: a test executable.
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code := provisionApply(t.Context(), false); code != 0 {
		log, _ := os.ReadFile(calls)
		t.Fatalf("apply exited %d before systemd's bus answered; systemctl was called with:\n%s", code, log)
	}
}
