package cli

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
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console/bridge"
	"github.com/discobox-ai/discobox/pkg/desktopstage"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

func writeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// testDesktopResolver is a Linux resolver with nothing named, no manifest,
// and an executable in a temporary directory.
func testDesktopResolver(t *testing.T) desktopResolver {
	t.Helper()
	return desktopResolver{
		goos:       "linux",
		executable: filepath.Join(t.TempDir(), "discobox"),
		stageRoot:  filepath.Join(t.TempDir(), "desktop"),
		manifest:   func() (desktopstage.Manifest, error) { return desktopstage.Manifest{}, desktopstage.ErrNoManifest },
	}
}

// The app is the one named, then (with no manifest) the one beside this
// executable (ADR 26-11-07-983 §3). PATH is not searched.
func TestDesktopResolveOrder(t *testing.T) {
	r := testDesktopResolver(t)
	onPath := t.TempDir()
	writeExecutable(t, filepath.Join(onPath, "discobox-desktop"))
	t.Setenv("PATH", onPath)
	if _, _, err := r.resolve(context.Background()); err == nil {
		t.Fatal("found an app on PATH")
	}

	beside := filepath.Join(filepath.Dir(r.executable), "discobox-desktop")
	writeExecutable(t, beside)
	if got, _, err := r.resolve(context.Background()); err != nil || got != beside {
		t.Fatalf("beside the CLI: got %q, %v, want %q", got, err, beside)
	}

	named := filepath.Join(t.TempDir(), "mine")
	writeExecutable(t, named)
	r.binary = named
	if got, _, err := r.resolve(context.Background()); err != nil || got != named {
		t.Fatalf("named: got %q, %v", got, err)
	}
	r.binary = filepath.Join(t.TempDir(), "missing")
	if _, _, err := r.resolve(context.Background()); err == nil {
		t.Fatal("a named app that does not exist was accepted")
	}
}

func TestDesktopResolveNamesTheWaysOut(t *testing.T) {
	_, _, err := testDesktopResolver(t).resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "build:desktop") || !strings.Contains(err.Error(), desktopBinaryEnv) {
		t.Fatalf("got %v, want an error naming build:desktop and %s", err, desktopBinaryEnv)
	}
}

// On macOS and Windows the desktop app is not yet supported: no release has
// one for them, so a CLI there says so rather than looking for one. An app
// named explicitly still runs.
func TestDesktopIsNotYetSupportedOffLinux(t *testing.T) {
	for goos, name := range map[string]string{"darwin": "macOS", "windows": "Windows"} {
		r := testDesktopResolver(t)
		r.goos = goos
		r.manifest = func() (desktopstage.Manifest, error) {
			t.Fatalf("%s consulted a manifest", goos)
			return desktopstage.Manifest{}, nil
		}
		writeExecutable(t, filepath.Join(filepath.Dir(r.executable), desktopAppName(goos)))
		_, _, err := r.resolve(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not yet supported on "+name) || !strings.Contains(err.Error(), "Linux") {
			t.Fatalf("%s: got %v, want not yet supported", goos, err)
		}
		named := filepath.Join(t.TempDir(), "mine")
		writeExecutable(t, named)
		r.binary = named
		if got, _, err := r.resolve(context.Background()); err != nil || got != named {
			t.Fatalf("%s, named: got %q, %v", goos, got, err)
		}
	}
}

// A release CLI downloads the app its manifest pins, verifies it, and runs it
// from its staging directory, ahead of anything beside it; a tampered download
// is refused.
func TestDesktopResolveStagesThePinnedApp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the desktop app is staged on Linux only")
	}
	archive := desktopArchive(t, "app")
	served := archive
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(served) }))
	t.Cleanup(server.Close)
	sum := sha256.Sum256(archive)
	manifest := desktopstage.Manifest{
		Version: "v9.9.9", OS: "linux", Arch: runtime.GOARCH, Command: "discobox-desktop",
		Asset: serverstage.Asset{Name: "app.tar.gz", URLs: []string{server.URL + "/app.tar.gz"}, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(archive))},
	}
	r := testDesktopResolver(t)
	r.manifest = func() (desktopstage.Manifest, error) { return manifest, nil }
	writeExecutable(t, filepath.Join(filepath.Dir(r.executable), "discobox-desktop"))

	served = desktopArchive(t, "tampered")
	manifest.Asset.Size = int64(len(served))
	if _, _, err := r.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered: got %v, want a digest refusal", err)
	}

	served = archive
	manifest.Asset.Size = int64(len(archive))
	got, release, err := r.resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if want := filepath.Join(r.stageRoot, manifest.Asset.SHA256, "discobox-desktop"); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if body, _ := os.ReadFile(got); string(body) != "app" {
		t.Fatalf("ran %q", body)
	}
}

// With no state directory there is nowhere this user's alone to keep the app,
// so a release CLI stages nothing rather than falling back to a shared one.
func TestDesktopResolveNeedsAStateDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the desktop app is staged on Linux only")
	}
	r := testDesktopResolver(t)
	r.stageRoot = ""
	r.manifest = func() (desktopstage.Manifest, error) {
		return desktopstage.Manifest{Version: "v1", OS: runtime.GOOS, Arch: runtime.GOARCH}, nil
	}
	if _, _, err := r.resolve(context.Background()); err == nil || !strings.Contains(err.Error(), "state directory") {
		t.Fatalf("got %v, want a refusal naming the state directory", err)
	}
}

func desktopArchive(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "discobox-desktop", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
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

// The app gets no discobox credential or server address from this process's
// environment, and keeps everything else, its own variables included.
func TestDesktopEnvCarriesNoDiscoboxCredentials(t *testing.T) {
	got := desktopEnv([]string{
		"PATH=/bin", "DISPLAY=:0", "DISCOBOX_TOKEN=secret", "DISCOBOX_SERVER=https://x",
		"discobox_token=secret", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app",
	})
	want := []string{"PATH=/bin", "DISPLAY=:0", "DISCOBOX_DESKTOP_DEBUG=1", "DISCOBOX_DESKTOP_BINARY=/app"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// What the app logs reaches the terminal with its control sequences escaped,
// a line at a time, however the writes are split.
func TestDesktopStderrIsTerminalSafe(t *testing.T) {
	var out bytes.Buffer
	w := &terminalSafeLines{w: &out}
	for _, chunk := range []string{"bridge ← {\"name\":\"a\x1b]52;c;", "aGk=\x07\"}\nnext\u009b", "2J\ttab"} {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	w.flush()
	got := out.String()
	if strings.ContainsAny(got, "\x1b\x07\u009b") {
		t.Fatalf("a control reached the terminal: %q", got)
	}
	if !strings.Contains(got, "\n") || !strings.Contains(got, "\t") || strings.Count(got, "\n") != 2 {
		t.Errorf("lines and tabs not kept: %q", got)
	}
}

// --print-manifest prints the desktop manifest this build was linked with, and
// a build linked with none says so rather than printing nothing: it is what
// release:verify reads to check a release CLI pins the archive the release
// built.
func TestDesktopPrintManifest(t *testing.T) {
	saved := desktopstage.DefaultManifest
	t.Cleanup(func() { desktopstage.DefaultManifest = saved })

	desktopstage.DefaultManifest = ""
	root := NewRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"desktop", "--print-manifest"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), desktopstage.ErrNoManifest.Error()) {
		t.Fatalf("no manifest: got %v, want %v", err, desktopstage.ErrNoManifest)
	}

	pinned := desktopstage.Manifest{
		Version: "v9.9.9", OS: "linux", Arch: "arm64", Command: "discobox-desktop",
		Asset: serverstage.Asset{
			Name:   "discobox-desktop-linux-arm64.tar.gz",
			URLs:   []string{"https://example.com/v9.9.9/discobox-desktop-linux-arm64.tar.gz"},
			SHA256: strings.Repeat("a", 64),
			Size:   42,
		},
	}
	encoded, err := desktopstage.EncodeManifest(pinned)
	if err != nil {
		t.Fatal(err)
	}
	desktopstage.DefaultManifest = encoded
	root = NewRootCommand()
	out.Reset()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"desktop", "--print-manifest"})
	if err := root.Execute(); err != nil {
		t.Fatalf("pinned: %v\n%s", err, out.String())
	}
	got, err := desktopstage.ParseManifest(out.Bytes())
	if err != nil {
		t.Fatalf("printed %q: %v", out.String(), err)
	}
	if !reflect.DeepEqual(got, pinned) {
		t.Fatalf("printed %+v, want %+v", got, pinned)
	}
}

// What the window opens on: the create card for --new and the harnesses page
// for --harnesses, as run and configure open the TUI; nothing beyond the list
// with neither. A discobox and a page together, or both pages, are refused
// before anything runs.
func TestDesktopOpening(t *testing.T) {
	a := &App{}
	for _, c := range []struct {
		newBox, harnesses bool
		want              *bridge.Start
	}{
		{false, false, nil},
		{true, false, &bridge.Start{Command: "new"}},
		{false, true, &bridge.Start{Command: "harnesses"}},
	} {
		opening, err := a.desktopOpening(nil, nil, c.newBox, c.harnesses)
		if got := opening.start(&apiDataSource{}); err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("--new=%v --harnesses=%v: %+v, %v; want %+v", c.newBox, c.harnesses, got, err, c.want)
		}
	}
	if _, err := a.desktopOpening(nil, []string{"sbx_01hq"}, true, false); err == nil {
		t.Error("a discobox and --new were both taken")
	}
	for _, args := range [][]string{{"desktop", "--new", "--harnesses"}, {"desktop", "one", "two"}} {
		root := NewRootCommand()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Errorf("%v ran", args)
		}
	}
}

// A discobox the window opens on is named by its key: the name the window
// lists the server selectSandbox found it on under, and its ID, so two
// servers listing one ID open the one that was asked for. One server lists
// its boxes under no name; a server the window does not list names none of
// its keys.
func TestDesktopOpeningNamesTheBoxsServer(t *testing.T) {
	home, work, elsewhere := &App{}, &App{}, &App{}
	many := &apiDataSource{servers: []*tuiServer{
		{server: &server{name: "home", primary: true, app: home}},
		{server: &server{name: "work", app: work}},
	}}
	for _, c := range []struct {
		ds   *apiDataSource
		app  *App
		want string
	}{
		{many, work, "work/sbx_1"},
		{many, home, "home/sbx_1"},
		{many, elsewhere, "sbx_1"},
		{&apiDataSource{}, home, "sbx_1"},
	} {
		got := (&desktopOpening{command: "attach", app: c.app, sandboxID: "sbx_1"}).start(c.ds)
		if got == nil || *got != (bridge.Start{Command: "attach", Box: c.want}) {
			t.Errorf("start %+v, want the box %q", got, c.want)
		}
	}
}
