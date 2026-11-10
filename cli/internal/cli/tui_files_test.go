package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A download never replaces what is already there: its name is claimed in
// the downloads directory first, numbered beside what is there the way a
// browser names a second download of the same file.
func TestClaimLocalNameNumbersBesideWhatIsThere(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"notes.txt", "notes (2).txt", ".env", "src"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, c := range []struct {
		name, want string
		dir        bool
	}{
		{"fresh.go", "fresh.go", false},
		{"notes.txt", "notes (3).txt", false},
		{".env", ".env (2)", false},
		{"src", "src (2)", true},
		{"my.d", "my.d", true},
		{"my.d", "my.d (2)", true},
	} {
		got, err := claimLocalName(root, c.name, c.dir, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("claimLocalName(%q) = %q, want %q", c.name, got, c.want)
		}
		if info, err := os.Lstat(filepath.Join(dir, got)); err != nil || info.IsDir() != c.dir {
			t.Errorf("claimLocalName(%q) left %v, %v; want it made, a directory %v", c.name, info, err, c.dir)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "notes.txt")); string(b) != "mine" {
		t.Fatalf("notes.txt = %q, want it untouched", b)
	}
}

// A download never lands in the home directory itself, where a box's file
// named .bashrc would be one the shell runs: ~/Downloads is made when there
// is none, and an XDG download directory that is home is passed over.
func TestDownloadsAreNeverTheHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DOWNLOAD_DIR", home)
	got, err := downloadsDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "Downloads"); got != want {
		t.Fatalf("downloadsDir() = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Fatalf("~/Downloads = %v, %v; want it made", info, err)
	}
	other := t.TempDir()
	t.Setenv("XDG_DOWNLOAD_DIR", other)
	if got, err := downloadsDir(); err != nil || got != other {
		t.Fatalf("downloadsDir() = %q, %v; want the XDG directory %q", got, err, other)
	}
}

// A download copies the box's file into a name of its own beside what is
// already there, and nowhere above the downloads directory.
func TestDownloadFileLandsBesideWhatIsThere(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	fake.files["/src/.bashrc"] = fakeFile{content: "echo planted"}
	downloads := t.TempDir()
	t.Setenv("XDG_DOWNLOAD_DIR", downloads)
	if err := os.WriteFile(filepath.Join(downloads, ".bashrc"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := app.apiClient()
	if err != nil {
		t.Fatal(err)
	}
	ds := &apiDataSource{app: app, client: client, projectID: "project-1"}
	got, err := ds.DownloadFile(t.Context(), "sbx_1", "/src/.bashrc")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(downloads, ".bashrc (2)"); got != want {
		t.Fatalf("downloaded to %q, want %q", got, want)
	}
	if b, _ := os.ReadFile(got); string(b) != "echo planted" {
		t.Fatalf("download holds %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(downloads, ".bashrc")); string(b) != "mine" {
		t.Fatalf("the file already there = %q, want it untouched", b)
	}
	if _, err := ds.DownloadFile(t.Context(), "sbx_1", "/src/.."); err == nil {
		t.Fatal("a name that climbs out of the downloads directory should be refused")
	}
}

// A command run for the window answers with what it wrote and how it exited,
// and leaves no session behind in the discobox.
func TestRunCommandReadsTheOutputAndRemovesTheSession(t *testing.T) {
	ds, created, deleted := execServer(t, func(command []string) (string, int64) {
		if strings.Join(command, " ") == "git rev-parse --show-toplevel" {
			return "/src\n", 0
		}
		return "", 128
	})
	got, err := ds.RunCommand(t.Context(), "sbx_1", "/src", []string{"git", "rev-parse", "--show-toplevel"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Stdout != "/src\n" || got.ExitCode != 0 {
		t.Fatalf("RunCommand = %+v, want the top level and exit 0", got)
	}
	if got, err := ds.RunCommand(t.Context(), "sbx_1", "", []string{"false"}); err != nil || got.ExitCode != 128 {
		t.Fatalf("RunCommand(false) = %+v, %v; want its exit code", got, err)
	}
	if n, d := len(created()), deleted(); n != 2 || len(d) != 2 {
		t.Fatalf("%d execs created, deleted %q; want each removed after", n, d)
	}
}

// A drop never lands on what is already in the folder: cp's copy would
// replace a file and merge into a directory (ADR 26-11-10-203).
func TestUploadFilesRefusesANameAlreadyInTheFolder(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	fake.files["/src/notes.txt"] = fakeFile{content: "the box's own"}
	local := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(local, []byte("from here"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	ds := &apiDataSource{app: app, client: client, projectID: "project-1"}
	err = ds.UploadFiles(t.Context(), "sbx_1", []string{local}, "/src")
	if err == nil || !strings.Contains(err.Error(), `"notes.txt" is already in "/src"`) {
		t.Fatalf("UploadFiles = %v, want a refusal naming what is there", err)
	}
	if got := fake.files["/src/notes.txt"].content; got != "the box's own" {
		t.Fatalf("the box's file was replaced: %q", got)
	}
	for _, call := range fake.calls {
		if strings.HasPrefix(call, "PUT") {
			t.Fatalf("nothing should be written: %q", fake.calls)
		}
	}
}

// What a listing names is drawn, so the data source makes it safe to draw;
// the path it is acted on by stays the box's own.
func TestListFilesMakesNamesSafeToDraw(t *testing.T) {
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"/src","entries":[
			{"path":"/src/report\u202efdp.sh","name":"report\u202efdp.sh","type":"file","size":1,"mode":420,"modifiedAt":"2026-11-10T00:00:00Z","version":"v1"},
			{"path":"/src/link","name":"link","type":"symlink","linkTarget":"\u001b[31mred","size":1,"mode":420,"modifiedAt":"2026-11-10T00:00:00Z","version":"v1"}]}`)
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	client, err := app.apiClient()
	if err != nil {
		t.Fatalf("api client: %v", err)
	}
	ds := &apiDataSource{app: app, client: client, projectID: "project-1"}
	_, entries, err := ds.ListFiles(t.Context(), "sbx_1", "/src")
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0]; got.Name != `report\u202efdp.sh` || got.Path != "/src/report\u202efdp.sh" {
		t.Fatalf("entry = %+v, want the name escaped and the path the box's", got)
	}
	if got := entries[1].Target; got != `\x1b[31mred` {
		t.Fatalf("target = %q, want its escape shown, not obeyed", got)
	}
}
