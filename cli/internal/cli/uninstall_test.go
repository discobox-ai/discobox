package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adrg/xdg"

	"github.com/discobox-ai/discobox/health"
	"github.com/discobox-ai/discobox/internal/filelock"
)

// uninstallHome points every directory the uninstall resolves into a temporary
// home, so a test never sees — or deletes — the machine's own.
func uninstallHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for name, value := range map[string]string{
		"HOME":            home,
		"USERPROFILE":     home,
		"LOCALAPPDATA":    filepath.Join(home, "AppData", "Local"),
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR": filepath.Join(home, "run"),
		// The temporary directory too: the uninstall looks through it, and the
		// developer's own is not this test's to clear.
		"TMPDIR": filepath.Join(home, "tmp"),
		"TMP":    filepath.Join(home, "tmp"),
		"TEMP":   filepath.Join(home, "tmp"),
	} {
		t.Setenv(name, value)
	}
	for _, name := range []string{"DISCOBOX_INSTALL_DIR", "DISCOBOX_DATA_DIR", "DISCOBOX_CONFIG_DIR", "DISCOBOX_CACHE_DIR", "DISCOBOX_STATE_DIR", "DISCOBOX_CONFIG_FILE", ImageCacheEnv} {
		t.Setenv(name, "")
	}
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	return home
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runUninstall(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append(args, "admin", "uninstall"))
	err := cmd.Execute()
	return out.String(), err
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Nested locations fold into the one holding them, missing ones are not
// listed, and a location that holds this command or a directory that is not
// Discobox's is kept however it came to be named.
func TestUninstallEntriesFoldAndProtect(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state", "discobox")
	home := filepath.Join(root, "home")
	bin := filepath.Join(root, "bin", "discobox")
	checkout := filepath.Join(root, "checkout")
	named := filepath.Join(root, "var", "lib", "discobox")
	for _, dir := range []string{filepath.Join(state, "cli"), filepath.Join(state, "images"), home, bin, checkout, named} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	mustWriteFile(t, filepath.Join(state, "images", "blob"), "12345")
	executable := filepath.Join(bin, "discobox")

	entries, kept := uninstallEntries([]uninstallLocation{
		{path: filepath.Join(state, "images"), what: "downloaded images"},
		{path: filepath.Join(state, "cli"), what: "CLI state"},
		{path: state},
		{path: filepath.Join(root, "missing", "discobox"), what: "server cache"},
		{path: home, what: "server data"},
		{path: bin, what: "tool configuration"},
		{path: filepath.Join(root, "checkout"), what: "server state and logs", env: "DISCOBOX_STATE_DIR"},
		{path: filepath.Join(root, "var", "lib", "discobox"), what: "server data", env: "DISCOBOX_DATA_DIR"},
	}, []string{executable}, []string{home})

	if len(entries) != 2 || entries[0].path != state || entries[1].path != named {
		t.Fatalf("entries = %+v, want the state directory and the discobox directory a variable named", entries)
	}
	if got, want := strings.Join(entries[0].what, "; "), "CLI state; downloaded images"; got != want {
		t.Fatalf("what = %q, want %q", got, want)
	}
	if entries[0].size != 5 {
		t.Fatalf("size = %d, want 5", entries[0].size)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %+v, want the home directory, the executable's, and the checkout a variable named", kept)
	}
	for _, k := range kept {
		if k.path != home && k.path != bin && k.path != checkout {
			t.Fatalf("kept %s, which is neither protected, holds the command, nor was named by a variable", k.path)
		}
	}
}

// Declining, or giving no answer at all, deletes nothing.
func TestUninstallWithoutYesDeletesNothing(t *testing.T) {
	home := uninstallHome(t)
	stagedServer := filepath.Join(home, ".local", "state", "discobox", "server", "v1", "discobox-server")
	mustWriteFile(t, stagedServer, "server")

	out, err := runUninstall(t, "n\n", "--server", "unix://"+filepath.Join(home, "none.sock"))
	if err != nil {
		t.Fatalf("decline: %v\n%s", err, out)
	}
	if !strings.Contains(out, filepath.Join(home, ".local", "state", "discobox")) || !strings.Contains(out, "downloaded servers") {
		t.Fatalf("the plan does not list the staged server:\n%s", out)
	}
	if !strings.Contains(out, "Nothing was deleted.") {
		t.Fatalf("declining does not say so:\n%s", out)
	}

	if out, err := runUninstall(t, "", "--server", "unix://"+filepath.Join(home, "none.sock")); err == nil {
		t.Fatalf("no answer on stdin succeeded:\n%s", out)
	}
	if !exists(stagedServer) {
		t.Fatal("a staged server was deleted without a yes")
	}
}

// A yes removes every listed location and the Include lines naming them,
// leaving the rest of the home directory and the user's own ssh_config alone.
func TestUninstallRemovesWhatItListed(t *testing.T) {
	home := uninstallHome(t)
	managed := filepath.Join(home, ".local", "state", "discobox", "cli", "ssh", "proj_1", "config")
	mustWriteFile(t, managed, "Host box\n")
	mustWriteFile(t, filepath.Join(home, ".local", "state", "discobox", "images", "blobs", "sha256", "a"), "layer")
	mustWriteFile(t, filepath.Join(home, ".local", "share", "discobox", "discobox.db"), "db")
	mustWriteFile(t, filepath.Join(home, ".config", "discobox", "servers.json"), "{}")
	mustWriteFile(t, filepath.Join(home, ".cache", "discobox", "images", "x"), "x")
	mustWriteFile(t, filepath.Join(home, ".config", "other", "keep"), "keep")
	sshConfig := filepath.Join(home, ".ssh", "config")
	mustWriteFile(t, sshConfig, "Include "+sshConfigPath(managed)+"\n\nHost mine\n  User me\n")

	out, err := runUninstall(t, "y\n", "--server", "unix://"+filepath.Join(home, "none.sock"))
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, path := range []string{
		filepath.Join(home, ".local", "state", "discobox"),
		filepath.Join(home, ".local", "share", "discobox"),
		filepath.Join(home, ".config", "discobox"),
		filepath.Join(home, ".cache", "discobox"),
	} {
		if exists(path) {
			t.Errorf("%s is still there:\n%s", path, out)
		}
	}
	if !exists(filepath.Join(home, ".config", "other", "keep")) {
		t.Fatal("the uninstall removed something that is not Discobox's")
	}
	data, err := os.ReadFile(sshConfig)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "Host mine\n  User me\n"; got != want {
		t.Fatalf("ssh_config = %q, want %q", got, want)
	}
}

// What the install scripts leave beside the command goes; the command, and
// anything else in its directory, stays.
func TestUninstallRemovesInstallerLeftovers(t *testing.T) {
	home := uninstallHome(t)
	bin := filepath.Join(home, "AppData", "Local", "Programs", "Discobox")
	t.Setenv("DISCOBOX_INSTALL_DIR", bin)
	temp := filepath.Join(home, "tmp")

	leftovers := []string{
		filepath.Join(bin, "discobox.exe.old"),
		filepath.Join(bin, "discobox.exe.0123456789abcdef0123456789abcdef.old"),
		filepath.Join(bin, ".discobox.exe.new"),
		filepath.Join(home, ".local", "bin", ".discobox.4242"),
		filepath.Join(home, ".local", "bin", ".discobox.new"),
		filepath.Join(temp, "discobox-install-0123456789abcdef", "discobox.exe"),
		filepath.Join(temp, serverStartupLockName),
	}
	for _, path := range leftovers {
		mustWriteFile(t, path, "old")
	}
	keep := []string{
		filepath.Join(bin, "discobox.exe"),
		filepath.Join(home, ".local", "bin", "discobox"),
		filepath.Join(home, ".local", "bin", ".discobox.notes"),
		filepath.Join(home, ".local", "bin", "discobox-other.old"),
	}
	for _, path := range keep {
		mustWriteFile(t, path, "keep")
	}

	out, err := runUninstall(t, "y\n", "--server", "unix://"+filepath.Join(home, "none.sock"))
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	for _, path := range leftovers {
		if exists(path) {
			t.Errorf("%s is still there:\n%s", path, out)
		}
	}
	for _, path := range keep {
		if !exists(path) {
			t.Errorf("%s was removed:\n%s", path, out)
		}
	}
}

// A local server that is running is asked to stop before its files are
// removed, and the files stay when it will not.
func TestUninstallStopsTheLocalServerFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the local server listens on a named pipe on Windows")
	}
	// A short path, made before the home redirects the temporary directory: a
	// unix socket's path is bounded, and a test's temporary directory can be
	// longer than that bound.
	dir, err := os.MkdirTemp("", "dbx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	home := uninstallHome(t)
	data := filepath.Join(home, ".local", "share", "discobox", "discobox.db")
	mustWriteFile(t, data, "db")
	// The server holds its data directory's lock until it has finished
	// stopping, well after it stopped answering.
	lock, err := filelock.TryAcquire(filepath.Join(filepath.Dir(data), serverSingletonLockName))
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second}
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case health.Path:
			if err := json.NewEncoder(w).Encode(health.Status{Status: health.StatusReady}); err != nil {
				t.Error(err)
			}
		case "/shutdown":
			if !exists(data) {
				t.Error("the server's files were removed before it was asked to stop")
			}
			stopped.Store(true)
			w.WriteHeader(http.StatusAccepted)
			go func() {
				_ = listener.Close()
				time.Sleep(300 * time.Millisecond)
				if !exists(data) {
					t.Error("the server's files were removed while it was still draining")
				}
				_ = lock.Release()
			}()
		default:
			http.NotFound(w, r)
		}
	})
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	out, err := runUninstall(t, "yes\n", "--server", "unix://"+socket)
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if !stopped.Load() {
		t.Fatalf("the running server was never asked to stop:\n%s", out)
	}
	if !strings.Contains(out, "Stop the running server at unix://"+socket) {
		t.Fatalf("the plan does not say the server will be stopped:\n%s", out)
	}
	if exists(data) {
		t.Fatalf("the server's data is still there:\n%s", out)
	}
}
