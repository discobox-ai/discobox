package desktopstage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/internal/filelock"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

type member struct {
	name, body, link string
	mode             int64
	dir              bool
}

func archiveOf(t *testing.T, members ...member) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		h := &tar.Header{Name: m.name, Mode: m.mode}
		switch {
		case m.dir:
			h.Typeflag = tar.TypeDir
		case m.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, m.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, int64(len(m.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(m.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// appArchive is a release's app: an executable and a file beside it, the
// executable carrying the version so two releases differ.
func appArchive(t *testing.T, version string) []byte {
	return archiveOf(t,
		member{name: "bin/", dir: true, mode: 0o755},
		member{name: "bin/discobox-desktop", body: "app " + version, mode: 0o755},
		member{name: "share/notice.txt", body: "notice", mode: 0o644},
		member{name: "current", link: "bin/discobox-desktop"},
	)
}

// serve publishes archives by path and counts the requests for each.
func serve(t *testing.T, archives map[string][]byte) (*httptest.Server, map[string]*atomic.Int32) {
	t.Helper()
	hits := map[string]*atomic.Int32{}
	for name := range archives {
		hits[name] = &atomic.Int32{}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		body, ok := archives[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hits[name].Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, hits
}

func manifestFor(server *httptest.Server, version, name string, pinned []byte) Manifest {
	sum := sha256.Sum256(pinned)
	return Manifest{
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Command: "bin/discobox-desktop",
		Asset: serverstage.Asset{
			Name:   name,
			URLs:   []string{server.URL + "/" + name},
			SHA256: hex.EncodeToString(sum[:]),
			Size:   int64(len(pinned)),
		},
	}
}

func TestStageDownloadsUnpacksAndReuses(t *testing.T) {
	archive := appArchive(t, "v1")
	server, hits := serve(t, map[string][]byte{"app.tar.gz": archive})
	m := manifestFor(server, "v1", "app.tar.gz", archive)
	root := t.TempDir()

	var reports []serverstage.Progress
	app, err := Stage(context.Background(), m, Options{Root: root, OnProgress: func(p serverstage.Progress) { reports = append(reports, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if !app.Downloaded {
		t.Fatal("first staging did not report a download")
	}
	if want := filepath.Join(root, m.Asset.SHA256, "bin", "discobox-desktop"); app.Path != want {
		t.Fatalf("path %q, want %q", app.Path, want)
	}
	if body, err := os.ReadFile(app.Path); err != nil || string(body) != "app v1" {
		t.Fatalf("app: %q, %v", body, err)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(app.Path); info.Mode().Perm() != 0o700 {
			t.Fatalf("app mode %v, want 0700", info.Mode().Perm())
		}
		if info, _ := os.Stat(filepath.Join(app.Dir, "share", "notice.txt")); info.Mode().Perm() != 0o600 {
			t.Fatalf("data file mode %v, want 0600", info.Mode().Perm())
		}
		if target, err := os.Readlink(filepath.Join(app.Dir, "current")); err != nil || target != "bin/discobox-desktop" {
			t.Fatalf("symlink: %q, %v", target, err)
		}
	}
	if len(reports) == 0 || !reports[len(reports)-1].Done {
		t.Fatalf("progress did not end with Done: %+v", reports)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	again, err := Stage(context.Background(), m, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.Downloaded || hits["app.tar.gz"].Load() != 1 {
		t.Fatalf("second staging downloaded again (%d requests)", hits["app.tar.gz"].Load())
	}
	assertNoLeftovers(t, root)
}

// A tampered asset is refused, and nothing it contained is left to run.
func TestStageRefusesATamperedAsset(t *testing.T) {
	pinned := appArchive(t, "v1")
	tampered := appArchive(t, "evil")
	server, _ := serve(t, map[string][]byte{"app.tar.gz": tampered})
	m := manifestFor(server, "v1", "app.tar.gz", pinned)
	// Same size, so it is the digest that refuses it rather than the length.
	m.Asset.Size = int64(len(tampered))
	root := t.TempDir()

	app, err := Stage(context.Background(), m, Options{Root: root})
	if err == nil {
		_ = app.Close()
		t.Fatal("a tampered asset was staged")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error %q does not say the digest did not match", err)
	}
	if _, err := os.Stat(filepath.Join(root, m.Asset.SHA256)); !os.IsNotExist(err) {
		t.Fatalf("the digest directory exists after a refused download: %v", err)
	}
	assertNoLeftovers(t, root)
	if entries, _ := os.ReadDir(filepath.Join(root, runningDir)); len(entries) != 0 {
		t.Fatalf("a refused staging left a running lock: %v", entries)
	}
}

// Two CLI versions keep two apps: a newer CLI's app lands beside the older
// one's, which stays while it runs and is pruned once nothing uses it.
func TestTwoVersionsKeepTwoApps(t *testing.T) {
	one, two := appArchive(t, "v1"), appArchive(t, "v2")
	server, _ := serve(t, map[string][]byte{"one.tar.gz": one, "two.tar.gz": two})
	m1 := manifestFor(server, "v1", "one.tar.gz", one)
	m2 := manifestFor(server, "v2", "two.tar.gz", two)
	root := t.TempDir()

	running, err := Stage(context.Background(), m1, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := Stage(context.Background(), m2, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, app := range []*App{running, newer} {
		if _, err := os.Stat(app.Path); err != nil {
			t.Fatalf("with both in use: %v", err)
		}
	}
	if running.Dir == newer.Dir {
		t.Fatal("two releases share a directory")
	}

	// The old one stops; the next staging of the new one prunes it.
	if err := running.Close(); err != nil {
		t.Fatal(err)
	}
	if err := newer.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Stage(context.Background(), m2, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := os.Stat(running.Dir); !os.IsNotExist(err) {
		t.Fatalf("an unused old app was not pruned: %v", err)
	}
	if _, err := os.Stat(again.Path); err != nil {
		t.Fatal(err)
	}
}

// A running lock whose holder died does not keep its app forever.
func TestPruneRemovesAppsWhoseHolderDied(t *testing.T) {
	root := t.TempDir()
	stale := strings.Repeat("a", 64)
	if err := os.MkdirAll(filepath.Join(root, stale, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, runningDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A lock file nobody holds is what a killed CLI leaves.
	lockPath := filepath.Join(root, runningDir, stale+".1-dead.lock")
	if err := os.WriteFile(lockPath, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	held := strings.Repeat("b", 64)
	if err := os.Mkdir(filepath.Join(root, held), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := filelock.TryAcquire(filepath.Join(root, runningDir, held+".2-live.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	abandoned := filepath.Join(root, stale+".staging-123")
	if err := os.Mkdir(abandoned, 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(root, "not-a-digest")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := Prune(context.Background(), root, ""); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Join(root, stale), abandoned, lockPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived the prune: %v", gone, err)
		}
	}
	for _, kept := range []string{filepath.Join(root, held), unrelated} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s was pruned: %v", kept, err)
		}
	}
}

func TestUnpackRefusesMembersOutsideTheApp(t *testing.T) {
	for name, data := range map[string][]byte{
		"parent path":   archiveOf(t, member{name: "../escape", body: "x", mode: 0o644}),
		"absolute path": archiveOf(t, member{name: "/etc/escape", body: "x", mode: 0o644}),
		"absolute link": archiveOf(t, member{name: "link", link: "/etc/passwd"}),
		"link out":      archiveOf(t, member{name: "bin/link", link: "../../outside"}),
		// Each link stays inside on its own path; through the first, the
		// second lands two levels above the app.
		"link under a link": archiveOf(t, member{name: "deep/a", link: "."}, member{name: "deep/a/b", link: "../.."}),
		"file under a link": archiveOf(t, member{name: "deep/a", link: "."}, member{name: "deep/a/f", body: "x", mode: 0o644}),
		"fifo":              fifoArchive(t),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, "a.tar.gz")
			if err := os.WriteFile(archive, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := unpack(archive, filepath.Join(dir, "out")); err == nil {
				t.Fatal("unpacked a member outside what an app is made of")
			}
		})
	}
}

func fifoArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "fifo", Typeflag: tar.TypeFifo}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestStageRefusesACommandTheArchiveLacks(t *testing.T) {
	archive := appArchive(t, "v1")
	server, _ := serve(t, map[string][]byte{"app.tar.gz": archive})
	m := manifestFor(server, "v1", "app.tar.gz", archive)
	m.Command = "bin/missing"
	if app, err := Stage(context.Background(), m, Options{Root: t.TempDir()}); err == nil {
		_ = app.Close()
		t.Fatal("staged an app whose command is not in it")
	}
}

func assertNoLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".staging-") || strings.Contains(entry.Name(), ".pruned-") {
			t.Fatalf("left behind %s", entry.Name())
		}
	}
}
