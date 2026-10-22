package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// fakeFilesAPI is a discobox's files API holding files in memory, by the path
// each was written at, and recording every call it was made.
type fakeFilesAPI struct {
	mu    sync.Mutex
	files map[string]fakeFile
	dirs  map[string]int64
	calls []string
}

// fakeFileStat is the stat the fake answers a call with.
type fakeFileStat struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       int64     `json:"mode"`
	ModifiedAt time.Time `json:"modifiedAt"`
	Version    string    `json:"version"`
}

type fakeFile struct {
	content string
	mode    string
}

func newFakeFilesAPI(t *testing.T) (*fakeFilesAPI, *App) {
	t.Helper()
	fake := &fakeFilesAPI{files: map[string]fakeFile{}, dirs: map[string]int64{}}
	server := httptest.NewServer(ignoringPortProbe(fake.serve))
	t.Cleanup(server.Close)
	return fake, &App{serverURL: server.URL, autoStart: autoStartServerFalse}
}

func (f *fakeFilesAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	const base = "/api/projects/project-1/sandboxes/sbx_1/files/"
	op, ok := strings.CutPrefix(r.URL.Path, base)
	if !ok {
		http.Error(w, `{"error":"not a files route"}`, http.StatusNotFound)
		return
	}
	query := r.URL.Query()
	f.calls = append(f.calls, r.Method+" "+op+" "+query.Encode())
	refuse := func(status int, path string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"`+path+` already exists"}`)
	}
	var path string
	switch {
	case r.Method == http.MethodGet && op == "stat":
		path = query.Get("path")
		_, file := f.files[path]
		_, dir := f.dirs[path]
		if !file && !dir {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
	case r.Method == http.MethodPut && op == "content":
		path = query.Get("path")
		if _, exists := f.files[path]; exists && query.Get("ifExists") == "fail" {
			refuse(http.StatusPreconditionFailed, path)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.files[path] = fakeFile{content: string(body), mode: query.Get("mode")}
	case r.Method == http.MethodPost && op == "mkdir":
		var req struct {
			Path string `json:"path"`
			Mode int64  `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		path = req.Path
		if _, exists := f.dirs[path]; exists {
			refuse(http.StatusConflict, path)
			return
		}
		f.dirs[path] = req.Mode
	default:
		http.Error(w, `{"error":"unexpected"}`, http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(path, "/") {
		path = "/home/agent/" + path
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(fakeFileStat{
		Path: path, Name: path[strings.LastIndex(path, "/")+1:], Type: "file",
		Mode: 0o644, ModifiedAt: time.Now().UTC(), Version: "v1",
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// A pasted image is written whole through the files API, as the request's
// body, and the path pasted is the one the write answered with.
func TestPutImageWritesThroughTheFilesAPI(t *testing.T) {
	fake, app := newFakeFilesAPI(t)
	data := strings.Repeat("\x89PNG", 64<<10)
	path, err := (&apiDataSource{app: app, projectID: "project-1"}).PutImage(t.Context(), "sbx_1", "20261022-shot.png", []byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if path != "/tmp/discobox-paste/20261022-shot.png" {
		t.Fatalf("path = %q", path)
	}
	if got := fake.files[path].content; got != data {
		t.Fatalf("wrote %d bytes, want the %d of the image", len(got), len(data))
	}
	if len(fake.calls) != 1 || !strings.Contains(fake.calls[0], "parents=true") {
		t.Fatalf("calls = %v, want one write that creates the directory", fake.calls)
	}
}

// A 409 on the way to the discobox — its pool unreachable, the discobox
// archived — is not a file already there, and says what it is; and a discobox
// whose agent predates the files API says that, rather than a decode error.
func TestFilesAPIRefusalsSayWhatTheyAre(t *testing.T) {
	status, body, contentType := http.StatusConflict, `{"error":"sandbox pool is not reachable"}`, "application/json"
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/files/stat") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"not found"}`)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
	mkdir := func() error {
		_, err := app.makeSandboxDirectory(t.Context(), "project-1", "sbx_1", &apiclientgen.SandboxFileMkdirRequest{Path: "/tmp/x"})
		return err
	}
	if err := mkdir(); err == nil || errors.Is(err, errSandboxFileExists) || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("mkdir refused on the way = %v, want the refusal, not a file already there", err)
	}
	body = "{\"error\":\"\x1b]52;c;aGk=\x07boom\"}"
	if err := mkdir(); err == nil || strings.ContainsRune(err.Error(), 0x1b) {
		t.Fatalf("mkdir error = %q, want the discobox's text escaped", err)
	}
	status, body, contentType = http.StatusNotFound, "404 page not found\n", "text/plain; charset=utf-8"
	if err := mkdir(); !errors.Is(err, errNoFilesAPI) {
		t.Fatalf("mkdir on an agent with no files API = %v, want errNoFilesAPI", err)
	}
}
