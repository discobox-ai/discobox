package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/sandboxtree"
	"github.com/discobox-ai/discobox/tarsums"
)

// exportDaemon is a Docker daemon holding no sandbox containers, whose one
// trick is the sandbox agent's export mode: a container created with the
// export entrypoint writes, on attach, the archive that mode would write of the
// trees it was mounted with. It is what lets these tests drive the real
// orchestration -- image check, mounts, attach, exit status, cleanup -- without
// a daemon.
type exportDaemon struct {
	mu sync.Mutex
	// labels are the image's; absent the export label, the image predates the
	// mode.
	labels map[string]string
	// fail makes the mode exit 1 without writing, saying so on stderr.
	fail string
	// created is the export container's create request, as the daemon got it.
	created *exportCreateRequest
	removed bool
}

type exportCreateRequest struct {
	Image           string
	Entrypoint      []string
	Cmd             []string
	Env             []string
	User            string
	Labels          map[string]string
	NetworkDisabled bool
	HostConfig      struct {
		NetworkMode    string
		ReadonlyRootfs bool
		CapDrop        []string
		CapAdd         []string
		Mounts         []struct {
			Source   string
			Target   string
			ReadOnly bool
		}
	}
}

func newExportDaemon() *exportDaemon {
	return &exportDaemon{labels: map[string]string{harness.TreeExportLabel: harness.TreeExportLabelValue}}
}

func (d *exportDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", "1.45")
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/_ping"):
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(path, "/containers/json"):
		writeJSON(w, []any{})
	case strings.Contains(path, "/images/") && strings.HasSuffix(path, "/json"):
		d.mu.Lock()
		labels := d.labels
		d.mu.Unlock()
		writeJSON(w, map[string]any{"Id": "sha256:export-image", "Config": map[string]any{"Labels": labels}})
	case strings.HasSuffix(path, "/containers/create"):
		var req exportCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.created = &req
		d.mu.Unlock()
		writeJSON(w, map[string]any{"Id": "export-1", "Warnings": []string{}})
	case strings.HasSuffix(path, "/attach"):
		d.attach(w)
	case strings.HasSuffix(path, "/start"):
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/wait"):
		status := 0
		if d.fail != "" {
			status = 1
		}
		writeJSON(w, map[string]any{"StatusCode": status})
	case r.Method == http.MethodDelete:
		d.mu.Lock()
		d.removed = true
		d.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case strings.Contains(path, "/containers/") && strings.HasSuffix(path, "/json"):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"No such container"}`))
	default:
		writeJSON(w, []any{})
	}
}

// attach hijacks the connection and writes the export mode's output as the
// daemon multiplexes it.
func (d *exportDaemon) attach(w http.ResponseWriter) {
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.multiplexed-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
	_ = buf.Flush()
	if d.fail != "" {
		_, _ = frameWriter{conn, stdcopy.Stderr}.Write([]byte(d.fail))
		return
	}
	d.mu.Lock()
	created := d.created
	d.mu.Unlock()
	archive := tarsums.NewWriter(frameWriter{conn, stdcopy.Stdout})
	writer := sandboxtree.NewWriter(archive)
	for _, subtree := range created.Cmd {
		for _, m := range created.HostConfig.Mounts {
			if (subtree == sandboxtree.Data && m.Target == sandboxDataMount) || (subtree == sandboxtree.Sources && m.Target == sandboxSourcesMount) {
				_ = writer.AddDir(context.Background(), m.Source, subtree, nil)
			}
		}
	}
	_ = archive.Close()
}

// frameWriter writes the daemon's multiplexed stream format: an 8-byte header
// naming the stream and the payload length, then the payload.
type frameWriter struct {
	w      io.Writer
	stream stdcopy.StdType
}

func (f frameWriter) Write(p []byte) (int, error) {
	header := [8]byte{byte(f.stream)}
	binary.BigEndian.PutUint32(header[4:], uint32(len(p)))
	if _, err := f.w.Write(header[:]); err != nil {
		return 0, err
	}
	return f.w.Write(p)
}

func writeJSON(w http.ResponseWriter, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(data)
}

// treeRuntime is a runtime whose Docker daemon holds no sandbox containers, so a
// sandbox is data and nothing else -- which is the state both halves of a
// transfer act on.
func treeRuntime(t *testing.T, projectID, poolID string) (*DockerSandboxRuntime, *exportDaemon) {
	t.Helper()
	daemon := newExportDaemon()
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &DockerSandboxRuntime{client: cli, projectID: projectID, poolID: poolID}, daemon
}

// treeFixture writes a sandbox tree under a relocated state root and returns
// the runtime addressing it.
func treeFixture(t *testing.T) (*DockerSandboxRuntime, string) {
	t.Helper()
	runtime, _, root := exportFixture(t)
	return runtime, root
}

func exportFixture(t *testing.T) (*DockerSandboxRuntime, *exportDaemon, string) {
	t.Helper()
	withTestRoot(t)
	runtime, daemon := treeRuntime(t, "project-1", "pool-1")
	root := runtime.sandboxRoot("sbx-1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return runtime, daemon, root
}

// testTreeImage is the pin an export is read with.
var testTreeImage = TreeImage{Name: "registry.example/harness:v1"}

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

	stream, err := runtime.ExportTree(t.Context(), "sbx-1", testTreeImage)
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

	stream, err := runtime.ExportTree(t.Context(), "sbx-1", testTreeImage)
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

	stream, err := source.ExportTree(t.Context(), "sbx-1", testTreeImage)
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
	destination, _ := treeRuntime(t, "project-2", "pool-2")
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
	if _, err := runtime.ExportTree(t.Context(), "sbx-unknown", testTreeImage); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The export container reads with the sandbox's own image and sees only the
// trees it is to read, read-only, with nowhere to send them but its stdout
// (ADR 0129 §1). It is not labeled a sandbox, so nothing listing this pool's
// sandboxes counts it, and it is gone when the export is.
func TestExportTreeRunsTheExportModeConfinedToTheTree(t *testing.T) {
	runtime, daemon, root := exportFixture(t)
	writeFile(t, filepath.Join(root, "data", "notes"), "hi\n", 0o644)
	writeFile(t, filepath.Join(root, "config", "sandbox.json"),
		`{"user":{"name":"ada","uid":1500,"gid":1600,"homeDirectory":"/home/ada"}}`, 0o644)

	stream, err := runtime.ExportTree(t.Context(), "sbx-1", testTreeImage)
	if err != nil {
		t.Fatal(err)
	}
	if got := entries(t, stream); got["data/notes"] != "hi\n" {
		t.Fatalf("data/notes = %q", got["data/notes"])
	}
	stream.Close()

	req := daemon.created
	if req == nil {
		t.Fatal("no export container was created")
	}
	if !slices.Equal(req.Entrypoint, []string{exportAgentPath, "export"}) {
		t.Errorf("entrypoint = %v, want the sandbox agent's export mode", req.Entrypoint)
	}
	// The sandbox has no sources yet, so only data is asked for: what is there
	// is what travels.
	if !slices.Equal(req.Cmd, []string{sandboxtree.Data}) {
		t.Errorf("subtrees = %v, want only the ones on disk", req.Cmd)
	}
	if req.Image != "sha256:export-image" {
		t.Errorf("image = %q, want the one the pin resolved to", req.Image)
	}
	if req.HostConfig.NetworkMode != "none" || !req.NetworkDisabled {
		t.Errorf("network = %q (disabled %v), want none", req.HostConfig.NetworkMode, req.NetworkDisabled)
	}
	if !req.HostConfig.ReadonlyRootfs || !slices.Equal(req.HostConfig.CapDrop, []string{"ALL"}) {
		t.Errorf("rootfs read-only %v, caps dropped %v; want a container that can only read", req.HostConfig.ReadonlyRootfs, req.HostConfig.CapDrop)
	}
	targets := map[string]bool{}
	for _, m := range req.HostConfig.Mounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is writable; the export reads", m.Target)
		}
		targets[m.Target] = true
	}
	if !targets[sandboxDataMount] || !targets[sandboxConfigMount] || len(targets) != 2 {
		t.Errorf("mounts = %v, want data and config only", targets)
	}
	// The user the sandbox boots as, so %HOME% resolves where boot put it.
	for _, want := range []string{"DISCOBOX_USER_UID=1500", "DISCOBOX_USER_GID=1600", "DISCOBOX_USER_NAME=ada", "DISCOBOX_USER_HOME=/home/ada"} {
		if !slices.Contains(req.Env, want) {
			t.Errorf("env %v lacks %s", req.Env, want)
		}
	}
	if _, ok := req.Labels[sandboxLabelManaged]; ok {
		t.Error("the export container is labeled a managed sandbox")
	}
	daemon.mu.Lock()
	removed := daemon.removed
	daemon.mu.Unlock()
	if !removed {
		t.Error("the export container was left behind")
	}
}

// An image whose sandbox agent predates the export mode would read the
// argument as an ordinary start, so it is refused before anything runs, with
// the remedy named (ADR 0129 §3).
func TestExportTreeRefusesAnImageWithoutTheExportMode(t *testing.T) {
	runtime, daemon, root := exportFixture(t)
	writeFile(t, filepath.Join(root, "data", "notes"), "hi\n", 0o644)
	daemon.labels = map[string]string{}

	_, err := runtime.ExportTree(t.Context(), "sbx-1", testTreeImage)
	if !errors.Is(err, ErrExportUnsupported) {
		t.Fatalf("err = %v, want ErrExportUnsupported", err)
	}
	if !strings.Contains(err.Error(), "discobox admin box upgrade") {
		t.Errorf("err = %q, want the remedy named", err)
	}
	if daemon.created != nil {
		t.Error("an export container was created for an image that cannot export")
	}
}

// A mode that fails before writing -- a manifest it cannot read, a user it
// cannot resolve -- is an error the caller gets as a status, with the mode's
// own reason in it, rather than an empty archive.
func TestExportTreeReportsAModeThatFailedAtOnce(t *testing.T) {
	runtime, daemon, root := exportFixture(t)
	writeFile(t, filepath.Join(root, "data", "notes"), "hi\n", 0o644)
	daemon.fail = "resolve sandbox identity: no such user"

	_, err := runtime.ExportTree(t.Context(), "sbx-1", testTreeImage)
	if err == nil {
		t.Fatal("a failed export mode was answered with a stream")
	}
	if !strings.Contains(err.Error(), "no such user") {
		t.Errorf("err = %q, want the mode's own reason", err)
	}
}

func TestExportTreeRefusesAnExportWithNoImage(t *testing.T) {
	runtime, _, root := exportFixture(t)
	writeFile(t, filepath.Join(root, "data", "notes"), "hi\n", 0o644)
	if _, err := runtime.ExportTree(t.Context(), "sbx-1", TreeImage{}); !errors.Is(err, ErrImageUnavailable) {
		t.Fatalf("err = %v, want ErrImageUnavailable", err)
	}
}

// The sandbox writes its own subtrees and nothing else. An origins entry from
// it would stand in for the pool's own bare repository, so it is refused, and
// the archive is left without the SHA256SUMS that would let anyone accept it.
func TestComposeTreeRefusesOriginsFromTheSandbox(t *testing.T) {
	var exported bytes.Buffer
	writer := tarsums.NewWriter(&exported)
	if err := writer.WriteHeader(&tar.Header{Name: "origins/primary.git/HEAD", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := composeTree(t.Context(), &out, &exported, filepath.Join(t.TempDir(), "origins")); err == nil {
		t.Fatal("an origins entry from the sandbox was carried")
	}
	reader := tarsums.NewReader(&out)
	for {
		if _, err := reader.Next(); errors.Is(err, io.EOF) {
			t.Fatal("the refused archive was closed with SHA256SUMS")
		} else if err != nil {
			break
		}
	}
}
