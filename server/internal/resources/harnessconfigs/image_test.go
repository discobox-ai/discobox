package harnessconfigs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/harness"
)

func TestParseImageMetadata(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		Env:     map[string]string{"HOME": "/home/sandbox"},
		Volumes: []harness.Volume{{Path: "/home/sandbox/.cache", Volume: harness.VolumeCache}},
		Harness: &harness.Image{
			ID: "codex", Name: "Codex", RunCommand: []string{"codex"},
			Secrets: []harness.Secret{{Name: "OPENAI_API_KEY", Required: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseImageMetadata("sha256:abc", map[string]string{harness.ImageLabel: string(label)})
	if err != nil {
		t.Fatalf("parse image metadata: %v", err)
	}
	if metadata.Digest != "sha256:abc" || metadata.Harness.ID != "codex" || len(metadata.Harness.Secrets) != 1 {
		t.Fatalf("metadata = %#v", metadata)
	}
	if len(metadata.Volumes) != 1 || metadata.Env["HOME"] != "/home/sandbox" {
		t.Fatalf("metadata env/volumes = %#v", metadata)
	}
}

func TestParseImageMetadataRejectsInvalidSecrets(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
		ID: "bad", Name: "Bad", RunCommand: []string{"bad"},
		Secrets: []harness.Secret{{Name: "not-valid"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseImageMetadata("sha256:abc", map[string]string{harness.ImageLabel: string(label)}); err == nil {
		t.Fatal("invalid secret name was accepted")
	}
}

// An omitted runCommand is a declaration: the sandbox resolves the user's login
// shell, which is the only place that knows what it is. `harness/shell` is the
// included image that makes that declaration. A blank one is still broken.
func TestParseImageMetadataAcceptsNoRunCommand(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		APIVersion:       harness.ImageAPIVersion,
		AdditionalGroups: []string{"docker"},
		Harness:          &harness.Image{ID: "shell", Name: "Shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseImageMetadata("sha256:shell", map[string]string{harness.ImageLabel: string(label)})
	if err != nil {
		t.Fatalf("parse shell image metadata: %v", err)
	}
	if len(metadata.Harness.RunCommand) != 0 || len(metadata.AdditionalGroups) != 1 {
		t.Fatalf("metadata = %#v", metadata)
	}

	blank, err := json.Marshal(harness.ImageMetadata{Harness: &harness.Image{
		ID: "shell", Name: "Shell", RunCommand: []string{"  "},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseImageMetadata("sha256:shell", map[string]string{harness.ImageLabel: string(blank)}); err == nil {
		t.Fatal("a blank runCommand was accepted")
	}
}

func TestParseImageMetadataRejectsUnknownVolumeKind(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		Volumes: []harness.Volume{{Path: "/data", Volume: "bogus"}},
		Harness: &harness.Image{ID: "codex", Name: "Codex", RunCommand: []string{"codex"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseImageMetadata("sha256:abc", map[string]string{harness.ImageLabel: string(label)}); err == nil {
		t.Fatal("unknown volume kind was accepted")
	}
}

// Every included image.json must be metadata this server accepts. The build
// compacts these files straight into the label, and a rejected label is a
// harness that is quietly skipped at seeding rather than an error anybody sees
// — so the authoring file is checked here, where a mistake fails loudly.
func TestIncludedImageJSONFilesAreValid(t *testing.T) {
	// server/ is a nested module; the harness folders live in the repo root.
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "harness", "*", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no harness image.json files found")
	}
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			t.Fatal(err)
		}
		compact := &bytes.Buffer{}
		if err := json.Compact(compact, data); err != nil {
			t.Fatalf("%s is not valid JSON: %v", match, err)
		}
		metadata, err := parseImageMetadata("sha256:test", map[string]string{harness.ImageLabel: compact.String()})
		if err != nil {
			t.Fatalf("%s would be rejected as a label: %v", match, err)
		}
		if metadata.APIVersion != harness.ImageAPIVersion {
			t.Errorf("%s apiVersion = %q, want %q", match, metadata.APIVersion, harness.ImageAPIVersion)
		}
		if metadata.Harness == nil {
			t.Errorf("%s declares no harness", match)
		}
		// Every harness image extends the sandbox-agent base, which ships the
		// desktop, and nothing merges the base's env into a harness manifest
		// (harness/DESIGN.md) — so each one has to declare DISPLAY itself.
		// Forgetting it is silent: every GUI tool in that sandbox fails with
		// "cannot open display" and nothing else says why.
		if got := metadata.Env["DISPLAY"]; got != ":0" {
			t.Errorf("%s env DISPLAY = %q, want \":0\"", match, got)
		}
	}
}
