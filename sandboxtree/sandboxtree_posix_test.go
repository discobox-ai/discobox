//go:build unix

package sandboxtree

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAddDirSkipsSocketsAndFifos(t *testing.T) {
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "real"), "x\n", 0o644)
	// A sandbox's home routinely holds both: an ssh-agent socket, a shell's
	// named pipe. Neither can be tarred and neither means anything afterwards.
	// Bound somewhere short and moved into place, because a temp path can be
	// past the 108 bytes a unix socket address allows.
	short, err := os.MkdirTemp("", "sock")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", filepath.Join(short, "s"))
	if err != nil {
		t.Fatalf("bind a unix socket: %v", err)
	}
	defer listener.Close()
	if err := os.Rename(filepath.Join(short, "s"), filepath.Join(data, "agent.sock")); err != nil {
		t.Fatalf("move the socket into the tree: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(data, "pipe"), 0o600); err != nil {
		t.Fatalf("create a fifo: %v", err)
	}

	got := entries(t, bytes.NewReader(archive(t, nil, map[string]string{Data: data})))
	if got["data/real"] != "x\n" {
		t.Errorf("data/real = %q", got["data/real"])
	}
	for _, name := range []string{"data/agent.sock", "data/pipe"} {
		if _, ok := got[name]; ok {
			t.Errorf("entry %q traveled; a socket or fifo cannot be restored", name)
		}
	}
}

// Two names, one inode: what pnpm's store does to a node_modules, and what
// must not be stored twice -- including across two subtrees of one archive.
func TestAddDirStoresAHardLinkOnce(t *testing.T) {
	root := t.TempDir()
	data, sources := filepath.Join(root, "data"), filepath.Join(root, "sources")
	writeFile(t, filepath.Join(data, "store", "pkg.js"), "module.exports = 1\n", 0o644)
	if err := os.MkdirAll(sources, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(data, "store", "pkg.js"), filepath.Join(sources, "pkg.js")); err != nil {
		t.Fatal(err)
	}
	got := entries(t, bytes.NewReader(archive(t, nil, map[string]string{Data: data, Sources: sources})))
	if got["sources/pkg.js"] != "<hardlink>data/store/pkg.js" {
		t.Errorf("sources/pkg.js = %q, want a hard link to the first name", got["sources/pkg.js"])
	}
}
