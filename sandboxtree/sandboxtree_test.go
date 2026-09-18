package sandboxtree

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/tarsums"
)

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// archive writes each directory under its archive name and returns the closed
// archive.
func archive(t *testing.T, skip func(string) bool, dirs map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	out := tarsums.NewWriter(&buf)
	writer := NewWriter(out)
	for _, name := range Subtrees {
		dir, ok := dirs[name]
		if !ok {
			continue
		}
		if err := writer.AddDir(t.Context(), dir, name, skip); err != nil {
			t.Fatal(err)
		}
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// entries reads an archive into a name -> content map, with directories and
// links recorded by their type. It reads through tarsums, so an archive
// without a matching SHA256SUMS fails the test.
func entries(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	out := map[string]string{}
	reader := tarsums.NewReader(r)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			out[header.Name] = "<dir>"
		case tar.TypeSymlink:
			out[header.Name] = "<symlink>" + header.Linkname
		case tar.TypeLink:
			out[header.Name] = "<hardlink>" + header.Linkname
		default:
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			out[header.Name] = string(body)
		}
	}
	return out
}

func TestAddDirNamesEntriesUnderTheSubtree(t *testing.T) {
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "home", "ada", ".bashrc"), "export PS1=x\n", 0o644)
	if err := os.Symlink(".bashrc", filepath.Join(data, "home", "ada", "rc")); err != nil {
		t.Fatal(err)
	}
	got := entries(t, bytes.NewReader(archive(t, nil, map[string]string{Data: data})))
	for name, want := range map[string]string{
		"data/":                 "<dir>",
		"data/home/ada/":        "<dir>",
		"data/home/ada/.bashrc": "export PS1=x\n",
		"data/home/ada/rc":      "<symlink>.bashrc",
	} {
		if got[name] != want {
			t.Errorf("entry %q = %q, want %q", name, got[name], want)
		}
	}
}

// What an image declares stays behind is left out whole: the directory and
// everything below it, and nothing beside it.
func TestAddDirLeavesOutWhatSkipNames(t *testing.T) {
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "var", "lib", "docker", "overlay2", "layer"), "big\n", 0o644)
	writeFile(t, filepath.Join(data, "var", "lib", "dockerfile-notes"), "keep\n", 0o644)
	writeFile(t, filepath.Join(data, "home", "ada", "work"), "keep\n", 0o644)
	skip := func(name string) bool { return name == "data/var/lib/docker" }

	got := entries(t, bytes.NewReader(archive(t, skip, map[string]string{Data: data})))
	for name := range got {
		if name == "data/var/lib/docker/" || strings.HasPrefix(name, "data/var/lib/docker/") {
			t.Errorf("entry %q traveled from a skipped directory", name)
		}
	}
	for _, name := range []string{"data/var/lib/dockerfile-notes", "data/home/ada/work"} {
		if got[name] != "keep\n" {
			t.Errorf("entry %q = %q; a sibling of a skipped path must travel", name, got[name])
		}
	}
}

// A file that changed length between the walk that sized it and the read that
// sends it ends the archive rather than going out zero-padded or cut: the
// entry's header is written by then, so the body cannot change length.
func TestCopyFileFailsOnAFileThatChangedLength(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		contents string
		promised int64
	}{
		"a file that shrank": {"short", 64},
		"a file that grew":   {"much longer than the header said", 4},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			writeFile(t, path, tc.contents, 0o644)
			var buf bytes.Buffer
			if err := copyFile(t.Context(), &buf, path, tc.promised); err == nil {
				t.Fatal("the export carried on with a file that is not the file on disk")
			}
		})
	}
}

// A file that vanished is the exception: cache files come and go, and failing
// a whole export because one was collected would be absurd. The entry still
// has to be filled to the length its header promised.
func TestCopyFilePadsAFileThatVanished(t *testing.T) {
	var buf bytes.Buffer
	if err := copyFile(t.Context(), &buf, filepath.Join(t.TempDir(), "gone"), 12); err != nil {
		t.Fatalf("a vanished file failed the export: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), make([]byte, 12)) {
		t.Errorf("wrote %q, want the 12 zero bytes the header promised", buf.Bytes())
	}
}

func TestEntryNameRejectsWhatIsNotASandboxTree(t *testing.T) {
	for _, name := range []string{
		"../x", "/x", "data/../../x", "", ".", `..\x`,
		// The pool's own, and what the create is about to write.
		".discobox-archived", "config/sandbox.json", "secrets/secrets.json",
	} {
		if _, err := EntryName(name); err == nil {
			t.Errorf("EntryName(%q) was accepted", name)
		}
	}
	got, err := EntryName("data/nested/file")
	if err != nil {
		t.Fatal(err)
	}
	if got != "data/nested/file" {
		t.Errorf("got %q, want a path relative to the tree", got)
	}
	if _, err := EntryName("origins/primary.git/HEAD", Data, Sources); err == nil {
		t.Error("an entry outside the named subtrees was accepted")
	}
}

func TestCopyReemitsWhatItIsGiven(t *testing.T) {
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "notes"), "hi\n", 0o644)
	src := archive(t, nil, map[string]string{Data: data})

	var buf bytes.Buffer
	dst := tarsums.NewWriter(&buf)
	if err := Copy(dst, tarsums.NewReader(bytes.NewReader(src)), Data, Sources); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	if got := entries(t, &buf); got["data/notes"] != "hi\n" || got["data/"] != "<dir>" {
		t.Fatalf("re-emitted %v", got)
	}
}

// The stream Copy passes on comes from a process the reader does not trust, so
// a name outside what it may write is refused rather than carried.
func TestCopyRefusesEntriesOutsideItsSubtrees(t *testing.T) {
	for name, header := range map[string]*tar.Header{
		"an origin from the sandbox": {Name: "origins/primary.git/HEAD", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"a config file":              {Name: "config/sandbox.json", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"an escape":                  {Name: "data/../../etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"a hard link out of bounds":  {Name: "data/stolen", Typeflag: tar.TypeLink, Linkname: "origins/x", Mode: 0o644},
	} {
		t.Run(name, func(t *testing.T) {
			var src bytes.Buffer
			writer := tarsums.NewWriter(&src)
			if err := writer.WriteHeader(header); err != nil {
				t.Fatal(err)
			}
			if header.Size > 0 {
				if _, err := writer.Write([]byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			dst := tarsums.NewWriter(io.Discard)
			if err := Copy(dst, tarsums.NewReader(&src), Data, Sources); err == nil {
				t.Fatal("the entry was accepted")
			}
		})
	}
}

// A stream that stopped short has no SHA256SUMS, and Copy must say so rather
// than end as though it were whole.
func TestCopyRefusesATruncatedStream(t *testing.T) {
	data := t.TempDir()
	writeFile(t, filepath.Join(data, "notes"), "hi\n", 0o644)
	src := archive(t, nil, map[string]string{Data: data})
	cut := src[:1536]
	if err := Copy(tarsums.NewWriter(io.Discard), tarsums.NewReader(bytes.NewReader(cut)), Data); !errors.Is(err, tarsums.ErrIncomplete) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want the stream refused as incomplete", err)
	}
}
