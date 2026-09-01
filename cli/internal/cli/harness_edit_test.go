package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
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
// fakeEditorEnv makes the test binary act as $EDITOR instead of running tests.
// Its value is written to the file being edited; empty leaves the file alone.
const fakeEditorEnv = "DISCOBOX_TEST_FAKE_EDITOR"

// TestMain lets the test binary stand in for the user's editor.
//
// The obvious stand-in is a shell script, but Windows cannot exec one, and the
// CLI runs the editor directly rather than through a shell
// (harness_edit.go's exec.CommandContext), so re-executing this binary is both
// portable and a faithful reproduction of how a real editor is launched.
func TestMain(m *testing.M) {
	if want, editing := os.LookupEnv(fakeEditorEnv); editing {
		if want != "" && len(os.Args) > 1 {
			if err := os.WriteFile(os.Args[1], []byte(want), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		os.Exit(0)
	}
	// A developer's machine is as often a WSL distribution as a plain Linux
	// one, and there every command that writes an ssh_config takes the WSL
	// bridge: it shells out to the real cmd.exe and writes into the real
	// Windows profile, which is neither what those tests mean nor something a
	// test may touch. The fake machine in tools_vscode_wsl_test.go sets these
	// back for the tests that are about the bridge.
	for _, name := range []string{"WSL_DISTRO_NAME", "WSL_INTEROP"} {
		if err := os.Unsetenv(name); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

// fakeEditor points $EDITOR at this test binary. write is the content the
// "editor" leaves in the file; empty means it exits without touching it.
func fakeEditor(t *testing.T, write string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", exe)
	t.Setenv(fakeEditorEnv, write)
}

func TestEditHarnessFileUpdatesConfiguredBucket(t *testing.T) {
	fakeEditor(t, "edited")

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
	fakeEditor(t, "")

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
