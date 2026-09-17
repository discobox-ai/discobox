package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
)

func TestListSandboxToolsListsTheImageThenTheSource(t *testing.T) {
	image, source := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(image, "10-diff.yaml"), []byte("key: d\nprogram: discobox-review\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(image, "diff"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "open.yaml"), []byte("runs: host\nprogram: open\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := &handler{tools: toolDirs{image: image, source: source}}
	got, err := h.ListSandboxTools(context.Background(), sandboxapi.ListSandboxToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 2 {
		t.Fatalf("tools = %+v", got.Tools)
	}
	diff, open := got.Tools[0], got.Tools[1]
	if diff.ID != "diff" || diff.Layer != sandboxapi.SandboxToolLayerImage || diff.Key.Value != "d" || diff.Problem.Set {
		t.Errorf("diff = %+v", diff)
	}
	// A source declaring a program for the client's machine is listed with
	// the reason it will not run there.
	if open.Layer != sandboxapi.SandboxToolLayerSource || open.Runs != sandboxapi.SandboxToolRunsHost || !open.Problem.Set {
		t.Errorf("open = %+v", open)
	}
}

func TestListSandboxToolsWithoutAWorkingTreeIsUnavailable(t *testing.T) {
	h := &handler{}
	if _, err := h.ListSandboxTools(context.Background(), sandboxapi.ListSandboxToolsParams{}); err == nil {
		t.Fatal("a sandbox whose tool directories never resolved listed tools")
	}
}
