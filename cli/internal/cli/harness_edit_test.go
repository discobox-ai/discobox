package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func editTestConfig() *apimodel.HarnessConfig {
	return &apimodel.HarnessConfig{
		ID:   "hc_1",
		Name: "Claude",
		Files: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: "settings.json", Content: "declared"},
		}),
		ConfiguredFiles: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".claude.json", Content: "configured", CreateOnly: apiclientgen.NewOptBool(true)},
			{Path: "settings.json", Content: "configured overlay"},
		}),
	}
}

// The configured overlay is what a sandbox actually gets, so a path present in
// both buckets must resolve to the configured entry.
func TestFindHarnessFilePrefersConfigured(t *testing.T) {
	cfg := editTestConfig()
	ref, ok := findHarnessFile(cfg, "settings.json")
	if !ok || ref.Bucket != harnessFileBucketConfigured {
		t.Fatalf("findHarnessFile(settings.json) = %+v, %v; want configured bucket", ref, ok)
	}
	if _, ok := findHarnessFile(cfg, "missing"); ok {
		t.Fatal("findHarnessFile should miss unknown paths")
	}
}

// fakeEditor writes a shell script that appends to the file it is given and
// points $EDITOR at it, standing in for an interactive editor.
func fakeEditor(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake editor: %v", err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", path)
}

func TestEditHarnessFileUpdatesConfiguredBucket(t *testing.T) {
	fakeEditor(t, `printf 'edited' > "$1"`)

	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects/project-1/harness-configs/hc_1" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode update body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"hc_1","projectId":"project-1","slug":"claude","name":"Claude","runCommand":["claude"],"configured":true,"builtIn":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	a := &App{}
	changed, err := a.editHarnessFile(context.Background(), client, "project-1", editTestConfig(), ".claude.json", nil, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("editHarnessFile: %v", err)
	}
	if !changed {
		t.Fatal("editHarnessFile should report the change")
	}

	if _, ok := gotBody["files"]; ok {
		t.Fatalf("editing a configured file must not touch the declared files: %v", gotBody)
	}
	files, ok := gotBody["configuredFiles"].([]any)
	if !ok || len(files) != 2 {
		t.Fatalf("configuredFiles = %v, want the full 2-entry set", gotBody["configuredFiles"])
	}
	edited := files[0].(map[string]any)
	if edited["path"] != ".claude.json" || edited["content"] != "edited" {
		t.Fatalf("edited entry = %v, want .claude.json with new content", edited)
	}
	if edited["createOnly"] != true {
		t.Fatalf("edited entry lost its createOnly flag: %v", edited)
	}
	if other := files[1].(map[string]any); other["content"] != "configured overlay" {
		t.Fatalf("sibling file content changed: %v", other)
	}
}

func TestEditHarnessFileNoChangeSkipsUpdate(t *testing.T) {
	fakeEditor(t, `true`)

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("unexpected request %s %s; unchanged edits must not call the API", r.Method, r.URL.Path)
	}))
	t.Cleanup(server.Close)
	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	a := &App{}
	changed, err := a.editHarnessFile(context.Background(), client, "project-1", editTestConfig(), "settings.json", nil, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatalf("editHarnessFile: %v", err)
	}
	if changed {
		t.Fatal("editHarnessFile should report no change")
	}
}

func TestEditHarnessFileUnknownPathListsFiles(t *testing.T) {
	a := &App{}
	_, err := a.editHarnessFile(context.Background(), nil, "project-1", editTestConfig(), "nope", nil, os.Stdout, os.Stderr)
	if err == nil {
		t.Fatal("expected an error for an unknown path")
	}
	for _, want := range []string{".claude.json", "settings.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should list available file %q", err, want)
		}
	}
}
