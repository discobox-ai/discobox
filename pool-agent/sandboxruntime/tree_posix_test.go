//go:build unix

package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestExportTreeSkipsSocketsAndFifos(t *testing.T) {
	runtime, root := treeFixture(t)
	writeFile(t, filepath.Join(root, "data", "real"), "x\n", 0o644)
	// A sandbox's home routinely holds both: an ssh-agent socket, a shell's
	// named pipe. Neither can be tarred and neither means anything afterwards.
	// Bound somewhere short and moved into place: a sandbox tree path is well
	// past the 108 bytes a unix socket address allows, so binding it directly
	// fails with EINVAL and the test skips itself into meaninglessness.
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
	socket := filepath.Join(root, "data", "agent.sock")
	if err := os.Rename(filepath.Join(short, "s"), socket); err != nil {
		t.Fatalf("move the socket into the tree: %v", err)
	}
	fifo := filepath.Join(root, "data", "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create a fifo: %v", err)
	}

	stream, err := runtime.ExportTree(t.Context(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := entries(t, stream)
	if got["data/real"] != "x\n" {
		t.Errorf("data/real = %q", got["data/real"])
	}
	for _, name := range []string{"data/agent.sock", "data/pipe"} {
		if _, ok := got[name]; ok {
			t.Errorf("entry %q traveled; a socket or fifo cannot be restored", name)
		}
	}
}

// A symlink an archive creates must not become a way for a later entry in the
// same archive to write outside the tree. The pool agent is root on the pool
// host, and an import is reachable by any project member, so the name that
// escapes is not the one with ".." in it -- it is `data/x` pointing at /etc
// followed by an ordinary-looking `data/x/passwd`.
func TestImportTreeDoesNotFollowSymlinksItJustCreated(t *testing.T) {
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	guarded := filepath.Join(victim, "passwd")
	writeFile(t, guarded, "root:x:0:0\n", 0o644)

	for name, entries := range map[string][]*tar.Header{
		"a file written through a symlinked directory": {
			{Name: "data/x", Typeflag: tar.TypeSymlink, Linkname: victim, Mode: 0o777},
			{Name: "data/x/planted", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		},
		"a directory created through a symlink, then chowned": {
			{Name: "data/x", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777},
			{Name: "data/x/planted", Typeflag: tar.TypeDir, Mode: 0o700, Uid: 4242, Gid: 4242},
		},
		"a hard link reaching a host file through a symlink": {
			{Name: "data/s", Typeflag: tar.TypeSymlink, Linkname: victim, Mode: 0o777},
			{Name: "data/stolen", Typeflag: tar.TypeLink, Linkname: "data/s/passwd", Mode: 0o644},
		},
		"an absolute symlink followed straight away": {
			{Name: "data/x", Typeflag: tar.TypeSymlink, Linkname: "/etc", Mode: 0o777},
			{Name: "data/x/planted", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		},
	} {
		t.Run(name, func(t *testing.T) {
			runtime, _ := treeFixture(t)
			var buf bytes.Buffer
			writer := tar.NewWriter(&buf)
			for _, header := range entries {
				if err := writer.WriteHeader(header); err != nil {
					t.Fatal(err)
				}
				if header.Size > 0 {
					if _, err := writer.Write([]byte("owned")); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}

			// Whether the restore fails or skips the entry is its business; what
			// must never happen is a write outside the tree.
			_ = runtime.ImportTree(t.Context(), "sbx-escape", &buf)

			if _, err := os.Stat(filepath.Join(victim, "planted")); !os.IsNotExist(err) {
				t.Errorf("a file was planted outside the sandbox tree: %v", err)
			}
			if _, err := os.Stat(filepath.Join(outside, "planted")); !os.IsNotExist(err) {
				t.Errorf("a directory was created outside the sandbox tree: %v", err)
			}
			info, err := os.Stat(guarded)
			if err != nil {
				t.Fatalf("the host file the archive aimed at is gone: %v", err)
			}
			if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Nlink != 1 {
				t.Errorf("a host file was hard-linked into the sandbox tree: nlink = %d", stat.Nlink)
			}
		})
	}
}
