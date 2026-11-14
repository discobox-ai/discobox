package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"
)

// fakeArchiveDaemon answers Docker's archive API for a few containers: a GET
// sends the tar it holds for the container (absent is Docker's 404), and a PUT
// records what was sent and where.
type fakeArchiveDaemon struct {
	mu     sync.Mutex
	layers map[string][]byte
	// mounts are each container's Docker mount destinations.
	mounts map[string][]string
	puts   []archivePut
}

type archivePut struct {
	container, path, copyUIDGID string
	body                        []byte
}

func (d *fakeArchiveDaemon) runtime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			w.WriteHeader(http.StatusOK)
			return
		}
		parts := strings.Split(r.URL.Path, "/")
		if len(parts) >= 3 && parts[len(parts)-1] == "json" && r.Method == http.MethodGet {
			id := parts[len(parts)-2]
			d.mu.Lock()
			_, ok := d.layers[id]
			mounts := d.mounts[id]
			d.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"No such container: ` + id + `"}`))
				return
			}
			var list []string
			for _, m := range mounts {
				list = append(list, `{"Type":"bind","Source":"/host`+m+`","Destination":"`+m+`"}`)
			}
			_, _ = w.Write([]byte(`{"Id":"` + id + `","Mounts":[` + strings.Join(list, ",") + `]}`))
			return
		}
		if len(parts) < 3 || parts[len(parts)-1] != "archive" {
			http.NotFound(w, r)
			return
		}
		id := parts[len(parts)-2]
		d.mu.Lock()
		defer d.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			layer, ok := d.layers[id]
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Could not find the file /workspace in container ` + id + `"}`))
				return
			}
			if got := r.URL.Query().Get("path"); got != "/workspace" {
				t.Errorf("archive read of %q, want the working root", got)
			}
			w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString([]byte(`{"name":"workspace","size":4096,"mode":2147484141}`)))
			w.Header().Set("Content-Type", "application/x-tar")
			_, _ = w.Write(layer)
		case http.MethodPut:
			body, _ := io.ReadAll(r.Body)
			d.puts = append(d.puts, archivePut{container: id, path: r.URL.Query().Get("path"), copyUIDGID: r.URL.Query().Get("copyUIDGID"), body: body})
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return testDockerRuntime(&DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1"})
}

// workingRootArchive is the tar Docker sends for /workspace: the directory
// itself, then each file under it.
func workingRootArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "workspace/", Typeflag: tar.TypeDir, Mode: 0o755, Uid: 1000, Gid: 1000}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "workspace/" + name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)), Uid: 1000, Gid: 1000}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// #1156: a box with no source, made before its agent kept the working root on
// the data root, has its work in its container's own layer. A rebuild removes
// that container, so what it holds at the working root is kept first and given
// to the next container, into its layer, before it starts, with the owners it
// had (ADR 26-11-14-302 §3).
func TestARemovedContainersWorkingRootIsGivenToTheNextContainer(t *testing.T) {
	layer := workingRootArchive(t, map[string]string{"notes.txt": "the user's work"})
	daemon := &fakeArchiveDaemon{layers: map[string][]byte{"ctr_old": layer}}
	r := daemon.runtime(t)
	ctx := context.Background()

	if err := r.stashWorkingRoot(ctx, "sbx_1", "ctr_old"); err != nil {
		t.Fatalf("stashWorkingRoot: %v", err)
	}
	stashed, err := os.ReadFile(r.workingRootStashPath("sbx_1"))
	if err != nil {
		t.Fatalf("nothing stashed: %v", err)
	}
	if !bytes.Equal(stashed, layer) {
		t.Fatalf("stash holds %d bytes, want the %d Docker sent, whole", len(stashed), len(layer))
	}

	if err := r.restoreWorkingRoot(ctx, "sbx_1", "ctr_new"); err != nil {
		t.Fatalf("restoreWorkingRoot: %v", err)
	}
	if len(daemon.puts) != 1 {
		t.Fatalf("archive writes = %d, want the stash given to the new container once", len(daemon.puts))
	}
	put := daemon.puts[0]
	if put.container != "ctr_new" || put.path != "/" || put.copyUIDGID != "true" {
		t.Fatalf("archive write = container %q path %q copyUIDGID %q; want ctr_new, /, with its owners", put.container, put.path, put.copyUIDGID)
	}
	if !bytes.Equal(put.body, layer) {
		t.Fatal("the new container was not given the archive its last one had")
	}
	if _, err := os.Stat(r.workingRootStashPath("sbx_1")); !os.IsNotExist(err) {
		t.Fatalf("stash still there after it was given (%v): the next boot would move it in twice", err)
	}

	// Nothing more to give the container after it.
	if err := r.restoreWorkingRoot(ctx, "sbx_1", "ctr_next"); err != nil || len(daemon.puts) != 1 {
		t.Fatalf("restore with no stash = %v, writes %d; want nothing", err, len(daemon.puts))
	}
}

// A layer with only the directory at the working root -- one whose agent moved
// its working root onto the data root, or a box that wrote nothing there -- has
// nothing to carry over, and a container with no working root at all neither.
func TestAnEmptyWorkingRootIsNotStashed(t *testing.T) {
	daemon := &fakeArchiveDaemon{layers: map[string][]byte{"ctr_empty": workingRootArchive(t, nil)}}
	r := daemon.runtime(t)
	for _, container := range []string{"ctr_empty", "ctr_none"} {
		if err := r.stashWorkingRoot(context.Background(), "sbx_1", container); err != nil {
			t.Fatalf("stashWorkingRoot(%s): %v", container, err)
		}
		if _, err := os.Stat(r.workingRootStashPath("sbx_1")); !os.IsNotExist(err) {
			t.Fatalf("%s: a stash was written (%v), want none", container, err)
		}
	}
}

// A stash not yet given to a container is what the sandbox's work is: the
// container being removed was built from it and never started, so its layer
// holds the same, and nothing overwrites it.
func TestAStashNotYetGivenIsKept(t *testing.T) {
	first := workingRootArchive(t, map[string]string{"notes.txt": "first"})
	daemon := &fakeArchiveDaemon{layers: map[string][]byte{
		"ctr_1": first,
		"ctr_2": workingRootArchive(t, map[string]string{"other.txt": "second"}),
	}}
	r := daemon.runtime(t)
	ctx := context.Background()
	if err := r.stashWorkingRoot(ctx, "sbx_1", "ctr_1"); err != nil {
		t.Fatal(err)
	}
	if err := r.stashWorkingRoot(ctx, "sbx_1", "ctr_2"); err != nil {
		t.Fatal(err)
	}
	stashed, err := os.ReadFile(r.workingRootStashPath("sbx_1"))
	if err != nil || !bytes.Equal(stashed, first) {
		t.Fatalf("stash = %d bytes (%v), want the first, untouched", len(stashed), err)
	}
}

// A container with a Docker mount at or above the working root -- one built
// before the pool stopped binding a source there itself -- has nothing of its
// own there: the archive would hold the mount's tree, carried into every
// container after it.
func TestAWorkingRootUnderADockerMountIsNotStashed(t *testing.T) {
	daemon := &fakeArchiveDaemon{
		layers: map[string][]byte{"ctr_mounted": workingRootArchive(t, map[string]string{"main.go": "a checkout"})},
		mounts: map[string][]string{"ctr_mounted": {"/workspace"}},
	}
	r := daemon.runtime(t)
	if err := r.stashWorkingRoot(context.Background(), "sbx_1", "ctr_mounted"); err != nil {
		t.Fatalf("stashWorkingRoot: %v", err)
	}
	if _, err := os.Stat(r.workingRootStashPath("sbx_1")); !os.IsNotExist(err) {
		t.Fatalf("a stash was written (%v), want none for a mounted working root", err)
	}
}

// The stash is the pool's, beside the sandbox's volumes: the box can write
// its data root, and a name there it replaced with a link would be followed
// by this agent as root on the host.
func TestTheStashIsKeptOutsideTheBoxsVolumes(t *testing.T) {
	r := (&fakeArchiveDaemon{}).runtime(t)
	stash := r.workingRootStashPath("sbx_1")
	for _, volume := range []string{r.sandboxDataRootPath("sbx_1"), r.sandboxConfigRoot("sbx_1"), r.sandboxSourcesRoot("sbx_1"), r.sandboxSecretsRoot("sbx_1")} {
		if rel, err := filepath.Rel(volume, stash); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Fatalf("stash %s is inside the box's volume %s", stash, volume)
		}
	}
}
