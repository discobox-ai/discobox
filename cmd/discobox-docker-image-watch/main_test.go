package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestDockerImageSpecsIncludeIndependentlyWatchedHarnesses(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	specs, err := dockerImageSpecs(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 5 {
		t.Fatalf("image specs = %d, want worker, sandbox, and three harnesses", len(specs))
	}
	for _, name := range []string{"codex", "claude-code", "opencode"} {
		var found *imageSpec
		for i := range specs {
			if specs[i].name == "harness-"+name {
				found = &specs[i]
				break
			}
		}
		if found == nil || !strings.Contains(strings.Join(found.buildArgs, " "), "--target "+name) {
			t.Fatalf("missing target-specific watcher for %s: %#v", name, found)
		}
		for _, file := range found.files {
			if strings.Contains(file, filepath.Join("image", "harnesses")) && !strings.Contains(file, filepath.Join("image", "harnesses", name)) {
				t.Fatalf("%s watcher includes unrelated metadata %s", name, file)
			}
		}
	}
}
