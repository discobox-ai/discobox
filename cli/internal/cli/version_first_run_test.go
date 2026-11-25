package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/endpoint"
	"github.com/discobox-ai/discobox/pkg/releasemanifest"
	"github.com/discobox-ai/discobox/pkg/serverstage"
	"github.com/discobox-ai/discobox/pkg/version"
)

// missingLocalServer is a local endpoint nothing listens on, of the kind this
// platform's local server uses: a named pipe on Windows, else a unix socket in
// a short directory, since a socket's path is bounded and a test's temporary
// directory can be longer than that bound.
func missingLocalServer(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("npipe:////./pipe/discobox-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return "unix://" + filepath.Join(shortSocketDir(t), "s.sock")
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "dbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// versionOnFreshInstall runs `discobox --version` against a local socket
// nothing listens on, with this state directory, the manifest named by
// manifest (none when empty) and the autolaunch set as autoLaunch says.
func versionOnFreshInstall(t *testing.T, state, manifest, autoLaunch string, args ...string) string {
	t.Helper()
	useTempServersFile(t)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(serverEnv, "")
	t.Setenv(ServerBinaryEnv, "")
	t.Setenv(ServerManifestEnv, manifest)
	t.Setenv(releasemanifest.Env, "")
	t.Setenv(AutoLaunchEnv, autoLaunch)
	t.Setenv(endpoint.AddressEnv, missingLocalServer(t))

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetArgs(append(args, "--version"))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute --version: %v", err)
	}
	return out.String()
}

// Right after an install nothing is downloaded, and the server's line says so
// and what fetches it, instead of a bare "unavailable" that reads as an error.
// Saying so downloads nothing.
func TestVersionHintsHowToStageServerOnFreshInstall(t *testing.T) {
	manifest, fetches := servedManifest(t, "v9.8.7")
	path := manifestFile(t, manifest)
	for _, tc := range []struct {
		autoLaunch string
		want       string
	}{
		{"1", "server version unavailable: not downloaded yet (the first command that needs it downloads it, or run `discobox admin server stage`)\n"},
		{"0", "server version unavailable: not downloaded yet (download it with `discobox admin server stage`, start it with `discobox admin server`)\n"},
	} {
		state := t.TempDir()
		got := versionOnFreshInstall(t, state, path, tc.autoLaunch)
		if want := "client version " + version.String() + "\n" + tc.want; got != want {
			t.Fatalf("autolaunch %s: --version printed %q, want %q", tc.autoLaunch, got, want)
		}
		if *fetches != 0 {
			t.Fatalf("--version downloaded the server %d times; it must only look", *fetches)
		}
		if entries, err := os.ReadDir(state); err == nil && len(entries) != 0 {
			t.Fatalf("--version wrote %v into the state directory; it must only look", entries)
		}
	}
}

// Once staged, the server is there and not running, and the line says that.
func TestVersionSaysAStagedServerIsNotRunning(t *testing.T) {
	manifest, _ := servedManifest(t, "v9.8.7")
	path := manifestFile(t, manifest)
	state := t.TempDir()
	if _, err := serverstage.Stage(context.Background(), manifest, serverstage.Options{Root: filepath.Join(state, "discobox", "server")}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		autoLaunch string
		want       string
	}{
		{"1", "server version unavailable: not running (the first command that needs it starts it)\n"},
		{"0", "server version unavailable: not running (start it with `discobox admin server`)\n"},
	} {
		got := versionOnFreshInstall(t, state, path, tc.autoLaunch)
		if want := "client version " + version.String() + "\n" + tc.want; got != want {
			t.Fatalf("autolaunch %s: --version printed %q, want %q", tc.autoLaunch, got, want)
		}
	}
	// --auto-start-server is the last word on which of the two it is.
	got := versionOnFreshInstall(t, state, path, "0", "--auto-start-server=true")
	if !strings.HasSuffix(got, "not running (the first command that needs it starts it)\n") {
		t.Fatalf("--auto-start-server=true --version printed %q, want the autolaunch's wording", got)
	}
}

// A build with nothing to stage from has no true advice to give, so the line
// stays bare: "stage it" would send somebody to a command that fails.
func TestVersionGivesNoHintWithNothingToStage(t *testing.T) {
	got := versionOnFreshInstall(t, t.TempDir(), "", "1")
	if want := "client version " + version.String() + "\nserver version unavailable\n"; got != want {
		t.Fatalf("--version printed %q, want %q", got, want)
	}
}

// A remote server that does not answer is not fixed by downloading one here.
func TestVersionGivesNoStagingHintForARemoteServer(t *testing.T) {
	manifest, _ := servedManifest(t, "v9.8.7")
	useTempServersFile(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(ServerBinaryEnv, "")
	t.Setenv(ServerManifestEnv, manifestFile(t, manifest))
	t.Setenv(releasemanifest.Env, "")
	t.Setenv(AutoLaunchEnv, "1")

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetArgs([]string{"--server", "http://127.0.0.1:1", "--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute --version: %v", err)
	}
	if want := "client version " + version.String() + "\nserver version unavailable\n"; out.String() != want {
		t.Fatalf("--version printed %q, want %q", out.String(), want)
	}
}

// Something listening that is not a Discobox server is not "not running": the
// hint is only for a probe that found nothing there at all.
func TestVersionGivesNoHintWhenSomethingElseAnswers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local server listens on a named pipe on Windows")
	}
	manifest, _ := servedManifest(t, "v9.8.7")
	state := t.TempDir()
	if _, err := serverstage.Stage(context.Background(), manifest, serverstage.Options{Root: filepath.Join(state, "discobox", "server")}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortSocketDir(t), "s.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not a discobox server"))
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	useTempServersFile(t)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(serverEnv, "")
	t.Setenv(ServerBinaryEnv, "")
	t.Setenv(ServerManifestEnv, manifestFile(t, manifest))
	t.Setenv(releasemanifest.Env, "")
	t.Setenv(AutoLaunchEnv, "1")
	t.Setenv(endpoint.AddressEnv, "unix://"+socket)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute --version: %v", err)
	}
	if want := "client version " + version.String() + "\nserver version unavailable\n"; out.String() != want {
		t.Fatalf("--version printed %q, want %q", out.String(), want)
	}
}
