package serverstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// assets serves a set of files and counts what was fetched, so a test can tell
// a staging that downloaded from one that reused what was already there.
type assets struct {
	files   map[string][]byte
	server  *httptest.Server
	fetched atomic.Int32
}

func serveAssets(t *testing.T, files map[string][]byte) *assets {
	t.Helper()
	a := &assets{files: files}
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		a.fetched.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(a.server.Close)
	return a
}

func (a *assets) manifest(version, command string, names ...string) Manifest {
	m := Manifest{
		Version: version,
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
		Command: command,
	}
	for _, name := range names {
		sum := sha256.Sum256(a.files[name])
		m.Assets = append(m.Assets, Asset{
			Name:       name,
			URL:        a.server.URL + "/" + name,
			SHA256:     hex.EncodeToString(sum[:]),
			Size:       int64(len(a.files[name])),
			Executable: name == command,
		})
	}
	return m
}

func TestStageVerifiesAndInstallsEveryAsset(t *testing.T) {
	served := serveAssets(t, map[string][]byte{
		"discobox-server": []byte("the server"),
		"relay.bin":       []byte("something else it needs"),
	})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server", "relay.bin")
	root := t.TempDir()

	dir, err := Stage(context.Background(), manifest, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "v1.2.3"); dir != want {
		t.Fatalf("staged into %q, want %q", dir, want)
	}
	for name, content := range served.files {
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(content) {
			t.Fatalf("%s = %q, want %q", name, got, content)
		}
	}
	// The record of where it came from, which is what makes the directory
	// complete and is otherwise unanswerable once the bytes are on disk.
	record, err := os.ReadFile(filepath.Join(dir, manifestFileName))
	if err != nil {
		t.Fatalf("no manifest was written beside the assets: %v", err)
	}
	var staged Manifest
	if err := json.Unmarshal(record, &staged); err != nil {
		t.Fatal(err)
	}
	if !manifest.sameAssets(staged) {
		t.Fatalf("the manifest written out describes different assets: %+v", staged)
	}
}

// The command is executable and nothing else is. A staged binary that cannot be
// run is a server that cannot start, and an asset that is executable without
// having asked to be is a file with rights it does not need.
func TestStageStagesTheCommandExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no mode bits; executability is by extension")
	}
	served := serveAssets(t, map[string][]byte{
		"discobox-server": []byte("the server"),
		"notes.txt":       []byte("not a program"),
	})
	dir, err := Stage(context.Background(),
		served.manifest("v1", "discobox-server", "discobox-server", "notes.txt"),
		Options{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"discobox-server": 0o700, "notes.txt": 0o600} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s is %o, want %o", name, got, want)
		}
	}
}

// A digest that does not match must leave nothing behind: the file it would
// leave is one that gets executed.
func TestStageKeepsNothingWhenADigestDoesNotMatch(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	manifest.Assets[0].SHA256 = strings.Repeat("a", 64)
	root := t.TempDir()

	_, err := Stage(context.Background(), manifest, Options{Root: root})
	if err == nil {
		t.Fatal("staging an asset with the wrong digest succeeded")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Fatalf("error does not say what was wrong: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed staging left %v behind", entries)
	}
}

// A second call is free, and a third with --force is not. The first is what
// every command that starts a server does; the second is the only thing that
// re-verifies a staged set.
func TestStageReusesAStagedSetUnlessForced(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()

	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	if got := served.fetched.Load(); got != 1 {
		t.Fatalf("first staging fetched %d assets, want 1", got)
	}
	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	if got := served.fetched.Load(); got != 1 {
		t.Fatalf("a staged version was downloaded again (%d fetches)", got)
	}
	if _, err := Stage(context.Background(), manifest, Options{Root: root, Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := served.fetched.Load(); got != 2 {
		t.Fatalf("--force fetched %d times in total, want 2", got)
	}
}

// The same version naming different contents is a different set, and reusing
// the directory would run the old one forever.
func TestStageRestagesWhenTheVersionWasRecut(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the first cut")})
	root := t.TempDir()
	if _, err := Stage(context.Background(), served.manifest("v1.2.3", "discobox-server", "discobox-server"), Options{Root: root}); err != nil {
		t.Fatal(err)
	}

	served.files["discobox-server"] = []byte("the second cut")
	dir, err := Stage(context.Background(), served.manifest("v1.2.3", "discobox-server", "discobox-server"), Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "discobox-server"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "the second cut" {
		t.Fatalf("staged %q, want the re-cut asset", got)
	}
}

// Versions live beside each other, which is what makes an upgrade something a
// running server survives and a rollback a directory that is still there.
func TestStageKeepsOneDirectoryPerVersion(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	root := t.TempDir()
	for _, version := range []string{"v1.2.3", "v1.3.0"} {
		if _, err := Stage(context.Background(), served.manifest(version, "discobox-server", "discobox-server"), Options{Root: root}); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"v1.2.3", "v1.3.0"} {
		if _, err := os.Stat(filepath.Join(root, version, "discobox-server")); err != nil {
			t.Fatalf("%s is not staged: %v", version, err)
		}
	}
}

func TestStageReportsProgress(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	var reports []Progress
	if _, err := Stage(context.Background(),
		served.manifest("v1", "discobox-server", "discobox-server"),
		Options{Root: t.TempDir(), OnProgress: func(p Progress) { reports = append(reports, p) }},
	); err != nil {
		t.Fatal(err)
	}
	if len(reports) < 2 {
		t.Fatalf("staging reported %d times, want a start and a finish", len(reports))
	}
	if first := reports[0]; first.Asset != "discobox-server" || first.Index != 1 || first.Assets != 1 {
		t.Fatalf("first report = %+v, want the one asset named", first)
	}
	if last := reports[len(reports)-1]; !last.Done {
		t.Fatalf("last report = %+v, want the closing one", last)
	}
}

// An asset the release does not have is a broken manifest, and the error has to
// say which file and where it was looked for.
func TestStageReportsAMissingAsset(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1", "discobox-server", "discobox-server")
	manifest.Assets[0].URL = served.server.URL + "/gone"
	_, err := Stage(context.Background(), manifest, Options{Root: t.TempDir()})
	if err == nil {
		t.Fatal("staging a missing asset succeeded")
	}
	for _, want := range []string{"discobox-server", "404"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestStagedRejectsAnIncompleteDirectory(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1", "discobox-server", "discobox-server")
	root := t.TempDir()
	dir, err := Stage(context.Background(), manifest, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := Staged(root, manifest); !ok {
		t.Fatal("a freshly staged set does not count as staged")
	}
	if err := os.Remove(filepath.Join(dir, "discobox-server")); err != nil {
		t.Fatal(err)
	}
	if _, ok := Staged(root, manifest); ok {
		t.Fatal("a directory missing the server counts as staged")
	}
}

// The whole point of declaring a size: GitHub serves a release asset with no
// Content-Length, so a download that asked the transport how big the file was
// got -1 and could only count upwards. The manifest's size is what the progress
// line counts towards, and it is there from the first report.
func TestStageReportsATotalWithoutAContentLength(t *testing.T) {
	body := []byte("the server, in a response with no declared length")
	sum := sha256.Sum256(body)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// What GitHub's asset CDN does: no Content-Length, chunked instead.
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	manifest := Manifest{
		Version: "v1", OS: runtime.GOOS, Arch: runtime.GOARCH, Command: "discobox-server",
		Assets: []Asset{{
			Name: "discobox-server", URL: server.URL + "/discobox-server",
			SHA256: hex.EncodeToString(sum[:]), Size: int64(len(body)), Executable: true,
		}},
	}
	var first Progress
	seen := false
	if _, err := Stage(context.Background(), manifest, Options{
		Root: t.TempDir(),
		OnProgress: func(p Progress) {
			if !seen && !p.Done {
				first, seen = p, true
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("staging reported nothing")
	}
	if first.Total != int64(len(body)) {
		t.Fatalf("first report total = %d, want the manifest's %d: the total must not come from the response", first.Total, len(body))
	}
}

// A size that does not match is reported as a size, not as a digest. Both catch
// it, but "is 41 bytes, not the 94371840 this build expects" is the legible
// complaint for a truncated download or a URL that now serves something else.
func TestStageReportsAWrongSizeAsASize(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1", "discobox-server", "discobox-server")
	manifest.Assets[0].Size = 999999
	root := t.TempDir()

	_, err := Stage(context.Background(), manifest, Options{Root: root})
	if err == nil {
		t.Fatal("staging an asset of the wrong size succeeded")
	}
	if !strings.Contains(err.Error(), "999999") || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("error %q does not name the size it expected", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed staging left %v behind", entries)
	}
}
