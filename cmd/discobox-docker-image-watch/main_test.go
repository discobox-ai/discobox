package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadDockerImageSpecs(t *testing.T) ([]imageSpec, string) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	specs, err := dockerImageSpecs(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return specs, repoRoot
}

func TestDockerImageSpecsBuildAllDevImagesAndUpdateEnv(t *testing.T) {
	specs, _ := loadDockerImageSpecs(t)
	if len(specs) != 5 {
		t.Fatalf("image specs = %d, want worker, sandbox, and three harnesses", len(specs))
	}

	gotNames := make([]string, 0, len(specs))
	gotEnvKeys := map[string]bool{}
	for _, spec := range specs {
		gotNames = append(gotNames, spec.name)
		if spec.envImageKey == "" {
			t.Fatalf("%s does not update an image reference in .env", spec.name)
		}
		gotEnvKeys[spec.envImageKey] = true
		if spec.envDigestKey != "" {
			gotEnvKeys[spec.envDigestKey] = true
		}
	}
	wantNames := []string{"pool-agent", "sandbox-agent", "harness-codex", "harness-claude-code", "harness-opencode"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("image build order = %#v, want %#v", gotNames, wantNames)
	}
	for _, key := range []string{
		"DISCOBOX_DOCKER_POOL_IMAGE",
		"DISCOBOX_DOCKER_POOL_IMAGE_DIGEST",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST",
		"DISCOBOX_HARNESS_CODEX_IMAGE",
		"DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE",
		"DISCOBOX_HARNESS_OPENCODE_IMAGE",
	} {
		if !gotEnvKeys[key] {
			t.Errorf("watcher does not update %s", key)
		}
	}
}

func TestDockerImageSpecsIncludeIndependentlyWatchedHarnesses(t *testing.T) {
	specs, repoRoot := loadDockerImageSpecs(t)
	sandboxDockerfile := filepath.Join(repoRoot, "sandbox-agent", "Dockerfile")
	for _, name := range []string{"codex", "claude-code", "opencode"} {
		var found *imageSpec
		for i := range specs {
			if specs[i].name == "harness-"+name {
				found = &specs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("missing watcher for %s", name)
		}
		args := strings.Join(found.buildArgs, " ")
		if !strings.Contains(args, filepath.Join("harness", harnessDir(name), "Dockerfile")) ||
			!strings.Contains(args, "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local") {
			t.Fatalf("watcher for %s does not use its harness Dockerfile and sandbox base: %#v", name, found)
		}
		if !contains(found.files, sandboxDockerfile) {
			t.Fatalf("%s watcher does not rebuild when the sandbox base Dockerfile changes", name)
		}
		for _, file := range found.files {
			if strings.HasSuffix(file, "image.json") && strings.Contains(file, string(filepath.Separator)+"harness"+string(filepath.Separator)) &&
				!strings.Contains(file, filepath.Join("harness", harnessDir(name), "image.json")) {
				t.Fatalf("%s watcher includes unrelated metadata %s", name, file)
			}
		}
	}
}

func TestUpdateEnvUpdatesAllImageReferencesWithoutDroppingUserValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("USER_SETTING=keep\nDISCOBOX_DEFAULT_SANDBOX_IMAGE=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DISCOBOX_DOCKER_POOL_IMAGE":            "discobox-pool-agent:dev-worker",
		"DISCOBOX_DOCKER_POOL_IMAGE_DIGEST":     "sha256:worker",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE":        "discobox-sandbox-agent:dev-sandbox",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST": "sha256:sandbox",
		"DISCOBOX_HARNESS_CODEX_IMAGE":          "discobox-harness-codex:dev-codex",
		"DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE":    "discobox-harness-claude-code:dev-claude",
		"DISCOBOX_HARNESS_OPENCODE_IMAGE":       "discobox-harness-opencode:dev-opencode",
	}
	if err := updateEnv(path, values); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	if !strings.Contains(env, "USER_SETTING=keep\n") {
		t.Fatalf("user value was not preserved:\n%s", env)
	}
	for key, value := range values {
		if !strings.Contains(env, key+"="+value+"\n") {
			t.Errorf("%s was not updated in .env:\n%s", key, env)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func harnessDir(name string) string {
	if name == "codex" {
		return "codex-cli"
	}
	return name
}
