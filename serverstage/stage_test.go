package serverstage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	if want := filepath.Join(root, runtime.GOOS+"-"+runtime.GOARCH, "v1.2.3"); dir != want {
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
	assertNothingLeftBehind(t, root)
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
		if _, err := os.Stat(filepath.Join(root, runtime.GOOS+"-"+runtime.GOARCH, version, "discobox-server")); err != nil {
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
	assertNothingLeftBehind(t, root)
}

// assertNothingLeftBehind is the property every failed staging has to have: no
// temporary directory, and above all no file anything could go on to execute.
func assertNothingLeftBehind(t *testing.T, root string) {
	t.Helper()
	var left []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			left = append(left, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("a failed staging left %v behind", left)
	}
}

// Staging for another machine must not evict this machine's own server: they
// are different sets under one version, and keyed by version alone each
// re-downloaded what the other had just deleted.
func TestStageKeepsPlatformsApart(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	root := t.TempDir()
	mine := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	theirs := mine
	theirs.OS, theirs.Arch = "plan9", "s390x"

	here, err := Stage(context.Background(), mine, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := Stage(context.Background(), theirs, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if here == elsewhere {
		t.Fatalf("both platforms staged into %s", here)
	}
	if _, ok := Staged(root, mine); !ok {
		t.Fatal("staging for another platform evicted this machine's server")
	}
}

// --force asks for the bytes to be fetched and checked again, so a commit it
// could not complete is a failure. Reporting the directory that was already
// there would answer the one question --force exists to ask with a set nothing
// re-verified.
func TestStageForceDoesNotReportAnUntouchedSetAsRestaged(t *testing.T) {
	requireModeBits(t)
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	root := t.TempDir()
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	// The parent of the staged directory, which commit has to rename within.
	parent := filepath.Dir(manifest.Dir(root))
	if err := os.Chmod(parent, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	if _, err := Stage(context.Background(), manifest, Options{Root: root, Force: true}); err == nil {
		t.Fatal("--force reported success without re-staging anything")
	}
}

// A body that stops arriving has to end the download rather than hang it. The
// CLI's root context is never canceled and the launch deadline is taken after
// staging runs, so without this a first `discobox run` sits on one status line
// for as long as the terminal is open.
func TestStageEndsADownloadThatStopsArriving(t *testing.T) {
	// Short enough to test, long enough that the ticker gets a look in.
	restore := stallTimeout
	stallTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stallTimeout = restore })

	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("the first few bytes"))
		w.(http.Flusher).Flush()
		<-blocked // and then nothing, ever
	}))
	defer func() { close(blocked); server.Close() }()

	manifest := Manifest{
		Version: "v1", OS: runtime.GOOS, Arch: runtime.GOARCH, Command: "discobox-server",
		Assets: []Asset{{
			Name: "discobox-server", URL: server.URL + "/discobox-server",
			SHA256: strings.Repeat("ab", 32), Size: 94 << 20, Executable: true,
		}},
	}
	root := t.TempDir()
	done := make(chan error, 1)
	go func() {
		_, err := Stage(context.Background(), manifest, Options{Root: root})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a download that stopped arriving succeeded")
		}
		if !strings.Contains(err.Error(), "stopped sending") {
			t.Fatalf("error %q does not say the download stalled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a download that stopped arriving hung instead of ending")
	}
	assertNothingLeftBehind(t, root)
}

// Ctrl-C skips the deferred cleanup, so the next run is what has to notice.
// Only leftovers old enough to be nobody's: a staging in flight has a
// directory of exactly this shape.
//
// The fixtures go where Stage actually creates its temporary — beside the
// destination — rather than where the sweep happens to look. An earlier version
// of this test planted them in the swept directory and passed while the sweep
// read a directory nothing was ever written to.
func TestStageSweepsAbandonedTemporaries(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()

	// Both places a temporary can be: beside the destination, where this
	// version creates one, and directly under the root, where the alphas did.
	where := filepath.Dir(manifest.Dir(root))
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(where, "v1.2.3.staging-999")
	legacy := filepath.Join(root, "v1.2.3.staging-888")
	fresh := filepath.Join(where, "v1.2.3.staging-111")
	for _, dir := range []string{stale, legacy, fresh} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * abandonedAge)
	for _, dir := range []string{stale, legacy} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{stale, legacy} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("an abandoned staging directory was left behind at %s: %v", dir, err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a staging that could still be in flight was deleted: %v", err)
	}
}

// The sweep has to read the directory Stage writes to. It did not: the
// temporary was created under the root while the sweep read the platform
// directory beside it, so the Ctrl-C leak the sweep exists for was untouched
// and a test that planted its own fixtures could not tell.
func TestStageSweepsWhereItCreatesItsTemporary(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()

	// Where a real staging puts its temporary, observed rather than assumed:
	// a download that never finishes leaves one in place to look at.
	restore := stallTimeout
	stallTimeout = 150 * time.Millisecond
	t.Cleanup(func() { stallTimeout = restore })
	blocked := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("some of it"))
		w.(http.Flusher).Flush()
		<-blocked
	}))
	defer func() { close(blocked); server.Close() }()

	stalling := manifest
	stalling.Assets = []Asset{{
		Name: "discobox-server", URL: server.URL + "/discobox-server",
		SHA256: strings.Repeat("ab", 32), Size: 94 << 20, Executable: true,
	}}
	seen := make(chan string, 1)
	go func() {
		for range 200 {
			if dir, ok := findStagingDir(root); ok {
				seen <- dir
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		seen <- ""
	}()
	_, _ = Stage(context.Background(), stalling, Options{Root: root})

	temp := <-seen
	if temp == "" {
		t.Fatal("no staging directory was ever observed")
	}
	if got, want := filepath.Dir(temp), filepath.Dir(manifest.Dir(root)); got != want {
		t.Fatalf("Stage creates its temporary in %s, but the sweep reads %s", got, want)
	}
}

// findStagingDir is the first in-flight temporary anywhere under root.
func findStagingDir(root string) (string, bool) {
	found := ""
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil //nolint:nilerr // a partial walk is all this needs
		}
		if d.IsDir() && strings.Contains(d.Name(), ".staging-") {
			found = path
		}
		return nil
	})
	return found, found != ""
}

// A download still arriving does not touch its directory's own timestamp —
// appending to a file inside it does not — so an old-looking directory whose
// contents are moving is somebody's work in progress, not an orphan.
func TestStageKeepsAStagingThatIsStillArriving(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()
	where := filepath.Dir(manifest.Dir(root))
	if err := os.MkdirAll(where, 0o700); err != nil {
		t.Fatal(err)
	}
	inFlight := filepath.Join(where, "v1.2.3.staging-inflight")
	if err := os.MkdirAll(inFlight, 0o700); err != nil {
		t.Fatal(err)
	}
	// The partial asset, written now; the directory entry itself is old.
	if err := os.WriteFile(filepath.Join(inFlight, "discobox-server"), []byte("half of it"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * abandonedAge)
	if err := os.Chtimes(inFlight, old, old); err != nil {
		t.Fatal(err)
	}

	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("a download that was still arriving was swept: %v", err)
	}
}

// v0.6.0-alpha.1 and .2 staged into <root>/<version>. Those directories hold a
// ~94 MB server, and nothing in the new layout addresses or removes them.
func TestStageMigratesTheLayoutTheAlphasStagedInto(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()

	// Exactly what an alpha left behind: <root>/<version>, manifest included.
	legacy := filepath.Join(root, "v1.2.3")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		manifestFileName:  record,
		"discobox-server": []byte("the server"),
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	dir, err := Stage(context.Background(), manifest, Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the old layout was left behind: %v", err)
	}
	if dir != manifest.Dir(root) {
		t.Fatalf("staged into %s, want %s", dir, manifest.Dir(root))
	}
	// Moved, not re-downloaded: the whole point is not paying for it twice.
	if got := served.fetched.Load(); got != 0 {
		t.Fatalf("the migrated set was downloaded again (%d fetches)", got)
	}
}

// v0.6.0-alpha.1 wrote no size — the field did not exist yet — so its
// manifest.json fails today's Validate. Migrating it must not mean deleting it:
// that record describes a complete, verified server, and alpha.1 is the release
// this migration is most for.
//
// The fixture is raw JSON on purpose. Marshaling today's struct produces
// alpha.2's shape and cannot catch this.
func TestStageMigratesARecordWrittenBeforeSizeExisted(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v0.6.0-alpha.1", "discobox-server", "discobox-server")
	root := t.TempDir()
	legacy := filepath.Join(root, "v0.6.0-alpha.1")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	// Exactly what alpha.1 wrote: no "size" anywhere.
	record := fmt.Sprintf(`{
  "version": "v0.6.0-alpha.1",
  "os": %q,
  "arch": %q,
  "command": "discobox-server",
  "assets": [
    {
      "name": "discobox-server",
      "url": "https://example.invalid/discobox-server",
      "sha256": %q,
      "executable": true
    }
  ]
}`, runtime.GOOS, runtime.GOARCH, manifest.Assets[0].SHA256)
	if _, err := ParseManifest([]byte(record)); err == nil {
		t.Fatal("this fixture is meant to be a record today's validator rejects")
	}
	for name, content := range map[string][]byte{
		manifestFileName:  []byte(record),
		"discobox-server": []byte("the server"),
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	migrateLegacyLayout(root)

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the old layout was left behind: %v", err)
	}
	moved := filepath.Join(root, runtime.GOOS+"-"+runtime.GOARCH, "v0.6.0-alpha.1", "discobox-server")
	if _, err := os.Stat(moved); err != nil {
		t.Fatalf("a verified alpha.1 server was not migrated (deleted?): %v", err)
	}
}

// A move that cannot happen leaves the set alone. Deleting it would turn a full
// disk into a machine with no server and — offline — no way to get one, and on
// Windows a partial delete leaves a directory this function can never see
// again.
func TestStageLeavesALegacySetItCannotMove(t *testing.T) {
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()
	legacy := filepath.Join(root, "v1.2.3")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		manifestFileName:  record,
		"discobox-server": []byte("the server"),
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A file where the platform directory needs to be, so MkdirAll fails.
	if err := os.WriteFile(filepath.Join(root, runtime.GOOS+"-"+runtime.GOARCH), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}

	migrateLegacyLayout(root)

	if _, err := os.Stat(filepath.Join(legacy, "discobox-server")); err != nil {
		t.Fatalf("a set that could not be moved was destroyed: %v", err)
	}
}

// A duplicate's bytes are not worth keeping, but whatever is left of it has to
// stay something this package can see. Deleting in place cannot promise that:
// a RemoveAll that stops at a locked file leaves a directory with no
// manifest.json to identify it and no .replaced- in its name to sweep it by.
//
// The unremovable child stands in for that: on Windows it is a running exe, and
// here it is a directory whose mode forbids unlinking what is inside while
// still allowing the directory itself to be renamed.
func TestStageMigrationLeavesADuplicateSweepable(t *testing.T) {
	requireModeBits(t)
	served := serveAssets(t, map[string][]byte{"discobox-server": []byte("the server")})
	manifest := served.manifest("v1.2.3", "discobox-server", "discobox-server")
	root := t.TempDir()

	// Already staged where it belongs, so the legacy copy is a duplicate.
	if _, err := Stage(context.Background(), manifest, Options{Root: root}); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, "v1.2.3")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		manifestFileName:  record,
		"discobox-server": []byte("the server"),
	} {
		if err := os.WriteFile(filepath.Join(legacy, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(legacy, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(legacy, 0o700) })

	migrateLegacyLayout(root)

	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("the duplicate is still at its old name: %v", err)
	}
	// Whatever survived is named so the sweep will keep trying.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == runtime.GOOS+"-"+runtime.GOARCH {
			continue
		}
		if !strings.Contains(name, ".replaced-") {
			t.Fatalf("%s survived under a name nothing sweeps", name)
		}
		_ = os.Chmod(filepath.Join(root, name), 0o700)
	}
}

// requireModeBits skips a test that provokes a filesystem failure by taking
// write permission away from a directory.
//
// Windows has no POSIX mode: os.Chmod there sets the read-only attribute at
// most and never stops a rename or an unlink, so the operation these tests need
// to fail simply succeeds and the assertion inverts — one of them failed on the
// Windows runner, and the other passed while testing nothing. Root is the same
// problem from the other side, since it ignores the bits entirely.
//
// What is skipped is the way of provoking the failure, not the behaviour: the
// code under test is platform-independent, and on Windows the case it stands in
// for is a locked executable rather than a mode.
func requireModeBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode, so chmod does not make a rename or an unlink fail")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this provokes the failure with")
	}
}
