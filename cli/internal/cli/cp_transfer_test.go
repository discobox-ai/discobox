package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/copyarchive"
)

// The fake remote exposes only files routes; an exec/SSH/key lookup fails.
func copyRemote(t *testing.T) (*App, *api.Client, map[string]string, map[string]int) {
	t.Helper()
	roots := map[string]string{"one": t.TempDir(), "two": t.TempDir()}
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 7 || parts[5] != "files" {
			t.Errorf("non-files route: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		root, ok := roots[parts[4]]
		if !ok {
			http.NotFound(w, r)
			return
		}
		op := parts[6]
		calls[r.Method+" "+op]++
		requested := r.URL.Query().Get("path")
		if requested == "~" {
			requested = ""
		}
		name := filepath.Join(root, strings.TrimPrefix(requested, "/"))
		writeStat := func() {
			w.Header().Set("Content-Type", "application/json")
			info, err := os.Stat(name)
			if err != nil {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(map[string]string{"error": "missing"})
				return
			}
			typ := api.SandboxFileTypeFile
			if info.IsDir() {
				typ = api.SandboxFileTypeDirectory
			}
			m := copyarchive.Info(info)
			w.Header().Set("Content-Type", "application/json")
			stat := &api.SandboxFileStat{Path: requested, Name: filepath.Base(name), Type: typ, Size: info.Size(), Mode: int64(m.Mode), ModifiedAt: m.ModifiedAt, AccessedAt: api.NewOptDateTime(m.AccessedAt)}
			identity, err := copyarchive.FileIdentity(name, info)
			if err != nil {
				t.Error(err)
			}
			stat.FileIdentity = api.NewOptString(identity)
			resolved, err := filepath.EvalSymlinks(name)
			if err == nil {
				stat.ResolvedPath = api.NewOptString(resolved)
			}
			data, err := stat.MarshalJSON()
			if err != nil {
				t.Error(err)
			}
			if _, err := w.Write(data); err != nil {
				t.Error(err)
			}
		}
		switch r.Method + " " + op {
		case "GET stat":
			writeStat()
		case "GET content":
			f, err := os.Open(name)
			if err != nil {
				http.Error(w, err.Error(), 404)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			io.Copy(w, f)
		case "PUT content":
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if err := os.WriteFile(name, b, 0644); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			writeStat()
		case "GET archive":
			w.Header().Set("Content-Type", "application/x-tar")
			if err := copyarchive.Export(r.Context(), w, name); err != nil {
				t.Errorf("export: %v", err)
			}
		case "PUT archive":
			if err := copyarchive.Import(r.Context(), r.Body, name, r.URL.Query().Get("preserve") == "true"); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			writeStat()
		case "POST metadata":
			var b api.SandboxFileMetadataRequest
			if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
				t.Error(err)
			}
			name = filepath.Join(root, b.Path)
			var mode *uint32
			if m, ok := b.Mode.Get(); ok {
				v := uint32(m)
				mode = &v
			}
			a, m := b.AccessedAt.Or(time.Now()), b.ModifiedAt.Or(time.Now())
			if err := copyarchive.SetMetadata(name, mode, &a, &m); err != nil {
				t.Error(err)
			}
			writeStat()
		default:
			t.Errorf("unexpected route %s %s", r.Method, op)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	client, err := api.NewClient(server.URL, api.WithClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return &App{serverURL: server.URL, autoStart: autoStartServerFalse}, client, roots, calls
}
func TestCopyFilesBinaryEmptyLiteralAndBetweenBoxes(t *testing.T) {
	app, client, roots, _ := copyRemote(t)
	local := t.TempDir()
	content := bytes.Repeat([]byte{0, 255, 1, 0}, 1<<18)
	source := filepath.Join(local, "spaces ; $(literal).bin")
	if err := os.WriteFile(source, content, 0640); err != nil {
		t.Fatal(err)
	}
	stamp := time.Unix(1700000000, 123456700)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{path: source}, {remote: true, reference: "one", path: "literal ; $.bin"}}, copyOptions{preserve: true}); err != nil {
		t.Fatal(err)
	}
	uploaded, err := os.Stat(filepath.Join(roots["one"], "literal ; $.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if meta := copyarchive.Info(uploaded); !meta.AccessedAt.Equal(stamp) || !meta.ModifiedAt.Equal(stamp) {
		t.Fatalf("upload precision lost: %+v", meta)
	}
	preserved := filepath.Join(local, "preserved")
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "one", path: "literal ; $.bin"}, {path: preserved}}, copyOptions{preserve: true}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(preserved)
	if err != nil {
		t.Fatal(err)
	}
	if meta := copyarchive.Info(info); !meta.AccessedAt.Equal(stamp) || !meta.ModifiedAt.Equal(stamp) {
		t.Fatalf("download precision lost: %+v", meta)
	}
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "one", path: "literal ; $.bin"}, {remote: true, reference: "two", path: "binary"}}, copyOptions{}); err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(local, "download")
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "two", path: "binary"}, {path: download}}, copyOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(download)
	if !bytes.Equal(got, content) {
		t.Fatal("binary changed")
	}
	if err := os.WriteFile(filepath.Join(roots["one"], "empty"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "one", path: "empty"}, {path: download}}, copyOptions{}); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(download)
	if info.Size() != 0 {
		t.Fatal("empty overwrite retained content")
	}
}
func TestCopyTreeUsesBatchAndMerges(t *testing.T) {
	app, client, roots, calls := copyRemote(t)
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "empty"), 0750); err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		if err := os.WriteFile(filepath.Join(source, strings.Repeat("x", i+1)), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{path: source}, {remote: true, reference: "one", path: "tree"}}, copyOptions{recursive: true}); err != nil {
		t.Fatal(err)
	}
	if calls["PUT archive"] != 1 || calls["PUT content"] != 0 {
		t.Fatalf("not batched: %v", calls)
	}
	if err := os.WriteFile(filepath.Join(roots["one"], "tree", "unrelated"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "one", path: "tree"}, {remote: true, reference: "two", path: "copied"}}, copyOptions{recursive: true}); err != nil {
		t.Fatal(err)
	}
	download := filepath.Join(t.TempDir(), "down")
	if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "two", path: "copied"}, {path: download}}, copyOptions{recursive: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(download, "empty")); err != nil {
		t.Fatal(err)
	}
	if calls["GET archive"] != 2 {
		t.Fatalf("not exported as batches: %v", calls)
	}
}

func TestCopyRefusesAliasesAndMissingDescendantsOfSource(t *testing.T) {
	app, client, roots, _ := copyRemote(t)
	if err := os.Mkdir(filepath.Join(roots["one"], "source"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(roots["one"], "source", "file"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("source", filepath.Join(roots["one"], "alias")); err != nil {
		t.Skip(err)
	}
	operands := []cpOperand{{remote: true, reference: "one", path: "source"}, {remote: true, reference: "one", path: "alias/missing/deeper"}}
	if err := app.copyOperands(t.Context(), client, "project", operands, copyOptions{recursive: true}); err == nil || !strings.Contains(err.Error(), "into itself") {
		t.Fatalf("self copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(roots["one"], "source", "missing")); !os.IsNotExist(err) {
		t.Fatal("self-copy refusal changed source")
	}
}

func TestCopyIncompleteBodyNeverPublishes(t *testing.T) {
	for _, size := range []int64{3, 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/stat") {
					w.Header().Set("Content-Type", "application/json")
					stat := &api.SandboxFileStat{Path: "file", Name: "file", Type: api.SandboxFileTypeFile, Size: size, Mode: 0644, ModifiedAt: time.Now()}
					data, err := stat.MarshalJSON()
					if err != nil {
						t.Error(err)
					}
					w.Write(data)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				io.WriteString(w, "short")
			}))
			defer server.Close()
			client, err := api.NewClient(server.URL, api.WithClient(server.Client()))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(target, []byte("before"), 0600); err != nil {
				t.Fatal(err)
			}
			app := &App{serverURL: server.URL, autoStart: autoStartServerFalse}
			if err := app.copyOperands(t.Context(), client, "project", []cpOperand{{remote: true, reference: "box", path: "file"}, {path: target}}, copyOptions{}); err == nil {
				t.Fatal("unstable source reported success")
			}
			got, _ := os.ReadFile(target)
			if string(got) != "before" {
				t.Fatalf("published incomplete file: %q", got)
			}
			entries, _ := os.ReadDir(filepath.Dir(target))
			if len(entries) != 1 {
				t.Fatalf("temporary files remain: %v", entries)
			}
		})
	}
}
