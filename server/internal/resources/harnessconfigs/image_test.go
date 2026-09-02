package harnessconfigs

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/harness"
)

// withBaseLayer adds the layer every harness image inherits from the sandbox
// agent base. Registration requires it as proof of lineage (ADR 0086 §1), so a
// label map without one is not a case any real image presents.
func withBaseLayer(own string) map[string]string {
	labels := map[string]string{
		harness.ImageLayerLabelPrefix + harness.SandboxBaseLayer: `{"apiVersion":"discobox.dev/image/v1","env":{"DISPLAY":":0"}}`,
	}
	if own != "" {
		labels[harness.ImageLabel] = own
	}
	return labels
}

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
	metadata, err := parseImageMetadata("sha256:abc", withBaseLayer(string(label)))
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
	if _, err := parseImageMetadata("sha256:abc", withBaseLayer(string(label))); err == nil {
		t.Fatal("invalid secret name was accepted")
	}
}

// An omitted runCommand is a declaration, not an omission: the image installs
// the conventional harness.RunCommand and the runtime types that (ADR 0086 §3).
// A blank one is still broken — "declares nothing" and "declares an empty
// string" are different, and only one of them is a decision.
func TestParseImageMetadataAcceptsNoRunCommand(t *testing.T) {
	label, err := json.Marshal(harness.ImageMetadata{
		APIVersion:       harness.ImageAPIVersion,
		AdditionalGroups: []string{"docker"},
		Harness:          &harness.Image{ID: "shell", Name: "Shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := parseImageMetadata("sha256:shell", withBaseLayer(string(label)))
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
	if _, err := parseImageMetadata("sha256:shell", withBaseLayer(string(blank))); err == nil {
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
	if _, err := parseImageMetadata("sha256:abc", withBaseLayer(string(label))); err == nil {
		t.Fatal("unknown volume kind was accepted")
	}
}

// Every included image.json must be metadata this server accepts, merged the
// way the built image will carry it: the base image's layer, then the harness's
// own. The build compacts these files straight into their labels, and a
// rejected label is a harness quietly skipped at seeding rather than an error
// anybody sees — so the authoring files are checked here, where a mistake fails
// loudly.
func TestIncludedImageJSONFilesAreValid(t *testing.T) {
	// server/ is a nested module; the image folders live in the repo root.
	repoRoot := filepath.Join("..", "..", "..", "..")
	baseLayer := compactFile(t, filepath.Join(repoRoot, "sandbox-agent", "image.json"))

	matches, err := filepath.Glob(filepath.Join(repoRoot, "harness", "*", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Plus the image that declares nothing at all and is its inherited layer,
	// which is `shell` and is also what any BYO image starts as.
	cases := map[string]string{"(no manifest)": ""}
	for _, match := range matches {
		cases[match] = compactFile(t, match)
	}
	for name, own := range cases {
		labels := map[string]string{harness.ImageLayerLabelPrefix + harness.SandboxBaseLayer: baseLayer}
		if own != "" {
			labels[harness.ImageLabel] = own
		}
		metadata, err := parseImageMetadata("sha256:test", labels)
		if err != nil {
			t.Errorf("%s would be rejected as a label: %v", name, err)
			continue
		}
		if metadata.APIVersion != harness.ImageAPIVersion {
			t.Errorf("%s apiVersion = %q, want %q", name, metadata.APIVersion, harness.ImageAPIVersion)
		}
		// The base image ships the socket-activated desktop, so every image
		// built on it needs DISPLAY. It is declared once, in the base layer,
		// and inherited — forgetting it is silent: every GUI tool in that
		// sandbox fails with "cannot open display" and nothing says why.
		if got := metadata.Env["DISPLAY"]; got != ":0" {
			t.Errorf("%s env DISPLAY = %q, want \":0\"", name, got)
		}
		// And the volumes, which a harness manifest no longer restates.
		if len(metadata.Volumes) == 0 {
			t.Errorf("%s declares no volumes, so its sandbox would persist nothing", name)
		}
	}
}

// A harness manifest must not restate what the base layer already declares.
// Restating is how the duplication ADR 0086 removed accumulated in the first
// place, and a stale copy silently outranks the base.
func TestHarnessManifestsDeclareNoBaseFacts(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("..", "..", "..", "..", "harness", "*", "image.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no harness image.json files found")
	}
	for _, match := range matches {
		var manifest harness.ImageMetadata
		if err := json.Unmarshal([]byte(compactFile(t, match)), &manifest); err != nil {
			t.Fatal(err)
		}
		if len(manifest.Env) != 0 || len(manifest.Volumes) != 0 || len(manifest.AdditionalGroups) != 0 {
			t.Errorf("%s restates base-layer facts: env=%v volumes=%v groups=%v",
				match, manifest.Env, manifest.Volumes, manifest.AdditionalGroups)
		}
		if manifest.Harness == nil {
			continue
		}
		// Naming a command is an override of the harness-run convention, and
		// nothing Discobox ships takes one (ADR 0086 §3).
		if len(manifest.Harness.RunCommand) != 0 || len(manifest.Harness.RelaunchCommand) != 0 {
			t.Errorf("%s overrides the harness-run convention: run=%v relaunch=%v",
				match, manifest.Harness.RunCommand, manifest.Harness.RelaunchCommand)
		}
	}
}

func compactFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	compact := &bytes.Buffer{}
	if err := json.Compact(compact, data); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return compact.String()
}
