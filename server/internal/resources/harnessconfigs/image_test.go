package harnessconfigs

import (
	"encoding/json"
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
