package copyarchive

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/tarsums"
)

func TestRoundTripMergeAndPreserve(t *testing.T) {
	source, dest := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	content := []byte{0, 1, 2, 255, 0}
	if err := os.WriteFile(filepath.Join(source, ".file"), content, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123000000)
	if err := os.Chtimes(filepath.Join(source, ".file"), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := Export(t.Context(), &archive, source); err != nil {
		t.Fatal(err)
	}
	if err := Import(t.Context(), bytes.NewReader(archive.Bytes()), dest, true); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, ".file"))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("content %v: %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "empty")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "unrelated")); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dest, ".file"))
	sourceInfo, err := os.Stat(filepath.Join(source, ".file"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != sourceInfo.Mode().Perm() || !info.ModTime().Equal(stamp) {
		t.Fatalf("metadata %v %s", info.Mode(), info.ModTime())
	}
}

func hostile(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	w := tarsums.NewWriter(&b)
	if err := w.WriteHeader(&tar.Header{Name: "files/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := io.CopyN(w, strings.NewReader(strings.Repeat("x", int(h.Size))), h.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestHostileArchives(t *testing.T) {
	for _, name := range []string{"../outside", "/outside", "C:/outside", "//host/outside", "files/../outside", "files/a\\outside", "files/./outside", "files//outside"} {
		t.Run(name, func(t *testing.T) {
			data := hostile(t, &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
			if err := Import(t.Context(), bytes.NewReader(data), t.TempDir(), false); err == nil {
				t.Fatal("accepted hostile name")
			}
		})
	}
	for _, typ := range []byte{tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo} {
		data := hostile(t, &tar.Header{Name: "files/escape", Typeflag: typ, Linkname: "../outside", Mode: 0644})
		if err := Import(t.Context(), bytes.NewReader(data), t.TempDir(), false); err == nil {
			t.Fatalf("accepted type %d", typ)
		}
	}
	data := hostile(t, &tar.Header{Name: "files/a", Typeflag: tar.TypeReg}, &tar.Header{Name: "files/a", Typeflag: tar.TypeReg})
	if err := Import(t.Context(), bytes.NewReader(data), t.TempDir(), false); err == nil {
		t.Fatal("accepted duplicate")
	}
}

func TestTruncatedCurrentFileKeepsPreviousAndRequiresManifest(t *testing.T) {
	dest := t.TempDir()
	target := filepath.Join(dest, "a")
	if err := os.WriteFile(target, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	data := hostile(t, &tar.Header{Name: "files/a", Typeflag: tar.TypeReg, Mode: 0644, Size: 1000})
	// Root directory, file header, and only part of the current file.
	if err := Import(t.Context(), bytes.NewReader(data[:1024+10]), dest, false); err == nil {
		t.Fatal("accepted truncation")
	}
	content, _ := os.ReadFile(target)
	if string(content) != "before" {
		t.Fatalf("published truncated content %q", content)
	}
	if err := Import(t.Context(), bytes.NewReader(data[:2560]), dest, false); err == nil {
		t.Fatal("accepted missing manifest")
	}
	entries, _ := os.ReadDir(dest)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".discobox-copy-") {
			t.Fatalf("temporary left: %s", entry.Name())
		}
	}
}
func TestSourceAndDestinationLinksCannotEscape(t *testing.T) {
	source, dest, outside := t.TempDir(), t.TempDir(), t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	if err := os.WriteFile(sentinel, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sentinel, filepath.Join(source, "escape")); err != nil {
		t.Skip(err)
	}
	var archive bytes.Buffer
	if err := Export(t.Context(), &archive, source); err == nil {
		t.Fatal("export read escaping link")
	}
	if bytes.Contains(archive.Bytes(), []byte("secret")) {
		t.Fatal("sentinel leaked")
	}
	if err := os.Symlink(sentinel, filepath.Join(dest, "a")); err != nil {
		t.Fatal(err)
	}
	data := hostile(t, &tar.Header{Name: "files/a", Typeflag: tar.TypeReg, Mode: 0644, Size: 1})
	if err := Import(t.Context(), bytes.NewReader(data), dest, true); err == nil {
		t.Fatal("import followed destination link")
	}
	content, _ := os.ReadFile(sentinel)
	if string(content) != "secret" {
		t.Fatal("sentinel changed")
	}
}
func TestInternalSourceLinkAndCycle(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("file", filepath.Join(source, "link")); err != nil {
		t.Skip(err)
	}
	var b bytes.Buffer
	if err := Export(t.Context(), &b, source); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := Import(t.Context(), &b, dest, false); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Lstat(filepath.Join(dest, "link"))
	if !info.Mode().IsRegular() {
		t.Fatal("source link was not dereferenced")
	}
	if err := os.Symlink(".", filepath.Join(source, "cycle")); err != nil {
		t.Fatal(err)
	}
	if err := Export(t.Context(), io.Discard, source); err == nil {
		t.Fatal("accepted cycle")
	}
}
func TestCancellationKeepsPreviousFile(t *testing.T) {
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "a"), []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := OpenDirectory(dest, false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteFile(ctx, root, "a", strings.NewReader("after"), Metadata{Mode: 0644}, false); err == nil {
		t.Fatal("accepted canceled write")
	}
	content, _ := os.ReadFile(filepath.Join(dest, "a"))
	if string(content) != "before" {
		t.Fatal("canceled write published")
	}
}

func TestSelectedRootMayHaveSymlinkedAncestors(t *testing.T) {
	parent := t.TempDir()
	actual := filepath.Join(parent, "real")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(actual, alias); err != nil {
		t.Skip(err)
	}
	if err := os.Mkdir(filepath.Join(actual, "source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actual, "source", "file"), []byte("content"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := Export(t.Context(), &archive, filepath.Join(alias, "source")); err != nil {
		t.Fatal(err)
	}
	if err := Import(t.Context(), &archive, filepath.Join(alias, "destination"), false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(actual, "destination", "file"))
	if err != nil || string(got) != "content" {
		t.Fatalf("copy %q: %v", got, err)
	}
	// The selected root itself is still not a destination link.
	if err := os.Symlink(filepath.Join(actual, "destination"), filepath.Join(actual, "redirect")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDirectory(filepath.Join(alias, "redirect"), false); err == nil {
		t.Fatal("destination root link accepted")
	}
}
