package sandboxruntime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// The credential environment the pool's broker answers with is the sandbox's
// secret environment less what its harness reads from a file: putting that
// sentinel back into a process's environment is what file delivery exists to
// prevent (harness.SecretDeliveryFile). Which those are is the pool's own
// record of what the control plane sent, never sandbox.json, which the sandbox
// can write.
func TestPublishCredentialEnvironmentLeavesOutFileDeliveredSecrets(t *testing.T) {
	r := &DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"}
	const sandboxID = "sb-1"
	if err := r.recordFileSecrets(sandboxID, []harness.Secret{
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", Delivery: harness.SecretDeliveryFile},
		{Name: "GH_TOKEN"},
	}); err != nil {
		t.Fatal(err)
	}
	// What the box could write says otherwise, and is not read.
	plantSandboxDocument(t, r, sandboxID, []harness.Secret{{Name: "GH_TOKEN", Delivery: harness.SecretDeliveryFile}})

	//nolint:gosec // G101: sentinels, which stand for a credential and are not one.
	secretEnv := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-SENTINEL", "GH_TOKEN": "ghp_SENTINEL", "NPM_TOKEN": "npm_SENTINEL"}
	if err := r.publishCredentialEnvironment(sandboxID, secretEnv); err != nil {
		t.Fatalf("publish: %v", err)
	}
	got := readCredentialEnvironment(t, r, sandboxID)
	want := map[string]string{"GH_TOKEN": "ghp_SENTINEL", "NPM_TOKEN": "npm_SENTINEL"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %v, want %v", got, want)
	}
	if _, ok := secretEnv["CLAUDE_CODE_OAUTH_TOKEN"]; !ok {
		t.Fatal("publishing changed the caller's map")
	}

	// None left removes the entry.
	if err := r.publishCredentialEnvironment(sandboxID, nil); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := readCredentialEnvironment(t, r, sandboxID); len(got) != 0 {
		t.Fatalf("environment = %v, want none", got)
	}
}

// A sandbox no record was written for is published nothing: which of its
// secrets are file-delivered is not known, and sandbox.json is not asked.
func TestPublishCredentialEnvironmentWithoutARecordPublishesNothing(t *testing.T) {
	r := &DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"}
	const sandboxID = "sb-1"
	plantSandboxDocument(t, r, sandboxID, nil)
	if err := r.publishCredentialEnvironment(sandboxID, map[string]string{"GH_TOKEN": "ghp_SENTINEL"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := readCredentialEnvironment(t, r, sandboxID); len(got) != 0 {
		t.Fatalf("environment = %v, want none without a record", got)
	}
}

func plantSandboxDocument(t *testing.T, r *DockerSandboxRuntime, sandboxID string, secrets []harness.Secret) {
	t.Helper()
	configDir := r.sandboxConfigRoot(sandboxID)
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sandboxconfig.Config{Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, sandboxDocumentName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCredentialEnvironment(t *testing.T, r *DockerSandboxRuntime, sandboxID string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(r.root.ProxySecretsFile(r.projectID, r.poolID))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Environment map[string]map[string]string `json:"environment"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Environment[sandboxID]
}
