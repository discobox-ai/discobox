package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/tarsums"
)

// treeRuntime is a runtime whose Docker daemon holds no containers, so a
// sandbox is data and nothing else -- which is the state both halves of a
// transfer act on.
func treeRuntime(t *testing.T, projectID, poolID string) *DockerSandboxRuntime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, projectID: projectID, poolID: poolID}
}

// treeFixture writes a sandbox tree under a relocated state root and returns
// the runtime addressing it.
func treeFixture(t *testing.T) (*DockerSandboxRuntime, string) {
	t.Helper()
	withTestRoot(t)
	runtime := treeRuntime(t, "project-1", "pool-1")
	root := runtime.sandboxRoot("sbx-1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime, root
}

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

// entries reads an archive into a name -> content map, with directories and
// symlinks recorded by their type so a test can assert on shape as well as
// bytes. It reads through tarsums, so an archive without a matching SHA256SUMS
// fails the test.
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

func TestExportTreeCarriesTheDurableSubtreesOnly(t *testing.T) {
	runtime, root := treeFixture(t)
	writeFile(t, filepath.Join(root, "data", ".bashrc"), "export PS1=x\n", 0o644)
	writeFile(t, filepath.Join(root, "sources", "primary", "main.go"), "package main\n", 0o600)
	writeFile(t, filepath.Join(root, "origins", "primary.git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	// Neither of these travels: both are rewritten by the create that follows a
	// restore, and both describe the pool they were written on.
	writeFile(t, filepath.Join(root, "config", "sandbox.json"), `{"pool":"pool-1"}`, 0o644)
	writeFile(t, filepath.Join(root, "secrets", "secrets.json"), `{"TOKEN":"sentinel"}`, 0o600)

	stream, err := runtime.ExportTree(t.Context(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	got := entries(t, stream)

	for name, want := range map[string]string{
		"data/.bashrc":             "export PS1=x\n",
		"sources/primary/main.go":  "package main\n",
		"origins/primary.git/HEAD": "ref: refs/heads/main\n",
	} {
		if got[name] != want {
			t.Errorf("entry %q = %q, want %q", name, got[name], want)
		}
	}
	for _, name := range []string{"config/sandbox.json", "secrets/secrets.json"} {
		if _, ok := got[name]; ok {
			t.Errorf("entry %q traveled; config and secrets are rewritten on restore", name)
		}
	}
}

func TestExportTreeSkipsAMissingSubtree(t *testing.T) {
	runtime, root := treeFixture(t)
	writeFile(t, filepath.Join(root, "data", "notes"), "hi\n", 0o644)

	stream, err := runtime.ExportTree(t.Context(), "sbx-1")
	if err != nil {
		t.Fatalf("a sandbox with no push-delivered source has no origins tree, which is not an error: %v", err)
	}
	defer stream.Close()
	if got := entries(t, stream); got["data/notes"] != "hi\n" {
		t.Fatalf("data/notes = %q", got["data/notes"])
	}
}

func TestTreeRoundTripPreservesModesSymlinksAndHardLinks(t *testing.T) {
	requirePOSIXHost(t)
	source, sourceRoot := treeFixture(t)
	writeFile(t, filepath.Join(sourceRoot, "data", "script.sh"), "#!/bin/sh\n", 0o755)
	writeFile(t, filepath.Join(sourceRoot, "data", "private"), "secret-ish\n", 0o600)
	if err := os.Symlink("script.sh", filepath.Join(sourceRoot, "data", "link")); err != nil {
		t.Fatal(err)
	}
	// Two names, one inode: what pnpm's store does to a node_modules, and what
	// must not be stored twice.
	if err := os.Link(filepath.Join(sourceRoot, "data", "script.sh"), filepath.Join(sourceRoot, "data", "also-script.sh")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sourceRoot, "data", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	stream, err := source.ExportTree(t.Context(), "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(stream)
	stream.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(t, bytes.NewReader(archive)); got["data/also-script.sh"] != "<hardlink>data/script.sh" &&
		got["data/script.sh"] != "<hardlink>data/also-script.sh" {
		t.Errorf("neither name was stored as a hard link to the other: %q, %q",
			got["data/script.sh"], got["data/also-script.sh"])
	}

	// Restore into a second pool, addressed the way the destination would.
	destination := treeRuntime(t, "project-2", "pool-2")
	if err := destination.ImportTree(t.Context(), "sbx-2", bytes.NewReader(archive)); err != nil {
		t.Fatal(err)
	}
	restored := destination.sandboxRoot("sbx-2")

	for path, mode := range map[string]os.FileMode{
		"data/script.sh": 0o755,
		"data/private":   0o600,
		"data/empty":     0o700,
	} {
		info, err := os.Stat(filepath.Join(restored, path))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s mode = %o, want %o", path, info.Mode().Perm(), mode)
		}
	}
	target, err := os.Readlink(filepath.Join(restored, "data", "link"))
	if err != nil {
		t.Fatal(err)
	}
	if target != "script.sh" {
		t.Errorf("symlink target = %q, want %q", target, "script.sh")
	}
	first, err := os.Stat(filepath.Join(restored, "data", "script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(restored, "data", "also-script.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(first, second) {
		t.Error("the hard link was restored as a separate file")
	}
}

// A file that changed length between the walk that sized it and the read that
// sends it ends the archive rather than going out zero-padded or cut: the
// entry's header is written by then, so the body cannot change length, and a
// git pack restored with a zero tail is discovered from inside the destination
// sandbox long after the source was archived.
//
// The size the header promised is the unit here, which is what the walk and the
// read disagree about; driving it through ExportTree instead would be racing
// the walk to rewrite the file.
func TestCopyTreeFileFailsOnAFileThatChangedLength(t *testing.T) {
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
			if err := copyTreeFile(t.Context(), &buf, path, tc.promised); err == nil {
				t.Fatal("the export carried on with a file that is not the file on disk")
			}
		})
	}
}

// A file that vanished is the exception: cache files under a home directory
// come and go, and failing a whole export because one was collected would be
// absurd. The entry still has to be filled to the length its header promised.
func TestCopyTreeFilePadsAFileThatVanished(t *testing.T) {
	var buf bytes.Buffer
	if err := copyTreeFile(t.Context(), &buf, filepath.Join(t.TempDir(), "gone"), 12); err != nil {
		t.Fatalf("a vanished file failed the export: %v", err)
	}
	if buf.Len() != 12 {
		t.Fatalf("wrote %d bytes, want the 12 the header promised", buf.Len())
	}
	if !bytes.Equal(buf.Bytes(), make([]byte, 12)) {
		t.Error("the padding is not zeros")
	}
}

func TestImportTreeRefusesAnExistingTree(t *testing.T) {
	runtime, root := treeFixture(t)
	writeFile(t, filepath.Join(root, "data", "keep"), "mine\n", 0o644)

	err := runtime.ImportTree(t.Context(), "sbx-1", bytes.NewReader(emptyTarArchive()))
	if !errors.Is(err, ErrTreeExists) {
		t.Fatalf("err = %v, want ErrTreeExists", err)
	}
	// And it left what was there alone.
	if data, readErr := os.ReadFile(filepath.Join(root, "data", "keep")); readErr != nil || string(data) != "mine\n" {
		t.Fatalf("the refused import disturbed the existing tree: %q, %v", data, readErr)
	}
}

func TestImportTreeRefusesEntriesOutsideTheSubtrees(t *testing.T) {
	runtime, _ := treeFixture(t)
	for _, name := range []string{
		"../escape",
		"/etc/passwd",
		"data/../../escape",
		// The archive marker and the config document are this pool's to write;
		// an archive carrying one would overwrite what the create produces.
		".discobox-archived",
		"config/sandbox.json",
	} {
		var buf bytes.Buffer
		writer := tarsums.NewWriter(&buf)
		if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := runtime.ImportTree(t.Context(), "sbx-escape", &buf); err == nil {
			t.Errorf("entry %q was accepted", name)
		}
	}
}

func TestImportTreeRemovesAPartialRestore(t *testing.T) {
	requirePOSIXHost(t)
	// Owned by whoever runs the test, so the restore gets as far as the end of
	// the archive rather than failing on a chown first.
	var buf bytes.Buffer
	writer := tarsums.NewWriter(&buf)
	if err := writer.WriteHeader(&tar.Header{Name: "data/good", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2, Uid: os.Getuid(), Gid: os.Getgid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "data/second", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3, Uid: os.Getuid(), Gid: os.Getgid()}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("two")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	whole := buf.Bytes()

	for name, tc := range map[string]struct {
		archive []byte
		want    error
	}{
		// The connection dropped inside a file's body.
		"cut inside a file": {whole[:512+1], io.ErrUnexpectedEOF},
		// The connection dropped between two files. A plain tar reader ends
		// here cleanly, so without the SHA256SUMS this archive restored as a
		// tree with one file missing and nothing to say so.
		"cut between files": {whole[:1024], tarsums.ErrIncomplete},
		// Every file arrived and only the checksums did not.
		"cut before the checksums": {whole[:2048], tarsums.ErrIncomplete},
		"a byte flipped": {func() []byte {
			flipped := bytes.Clone(whole)
			flipped[512] ^= 0xff
			return flipped
		}(), tarsums.ErrMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			runtime, _ := treeFixture(t)
			if err := runtime.ImportTree(t.Context(), "sbx-partial", bytes.NewReader(tc.archive)); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// Half a tree would be adopted by a create as readily as a whole one.
			if _, err := os.Stat(runtime.sandboxRoot("sbx-partial")); !os.IsNotExist(err) {
				t.Fatalf("the partial tree was left behind: %v", err)
			}
		})
	}
}

func TestExportTreeRefusesAMissingSandbox(t *testing.T) {
	runtime, _ := treeFixture(t)
	if _, err := runtime.ExportTree(t.Context(), "sbx-unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// treeEntryName is not the containment check -- os.Root is -- but it is what
// keeps an archive from carrying a file into one of the subtrees this pool
// writes for itself.
func TestTreeEntryNameRejectsWhatIsNotASandboxTree(t *testing.T) {
	for _, name := range []string{
		"../x", "/x", "data/../../x", "", ".", `..\x`,
		// This pool's own, and what the create is about to write.
		".discobox-archived", "config/sandbox.json", "secrets/secrets.json",
	} {
		if _, err := treeEntryName(name); err == nil {
			t.Errorf("treeEntryName(%q) was accepted", name)
		}
	}
	got, err := treeEntryName("data/nested/file")
	if err != nil {
		t.Fatal(err)
	}
	if got != "data/nested/file" {
		t.Errorf("got %q, want a path relative to the tree", got)
	}
}
