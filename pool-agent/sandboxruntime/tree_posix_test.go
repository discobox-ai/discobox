//go:build unix

package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/discobox-ai/discobox/tarsums"
)

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
			writer := tarsums.NewWriter(&buf)
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
