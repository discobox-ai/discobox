package terminal

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
)

//nolint:gosec // A path, not a credential.
const credentialsPath = ".claude/.credentials.json"

// credentialsTemplate is the shape harness/claude-code/configure.sh emits: the
// sentinel in accessToken, a placeholder refresh token, and a far-future expiry.
//
//nolint:gosec // A template and a placeholder, not credentials.
const credentialsTemplate = `{
  "claudeAiOauth": {
    "accessToken": "{{ .secrets.CLAUDE_CODE_OAUTH_TOKEN }}",
    "refreshToken": "discobox-refresh-happens-in-the-control-plane",
    "expiresAt": 4102444800000,
    "scopes": ["user:inference"],
    "subscriptionType": "max"
  }
}`

func credentialsHarness() config.Harness {
	return config.Harness{
		ID: "harness_test",
		Files: []config.HarnessFile{
			{Path: credentialsPath, Template: true, Content: credentialsTemplate},
		},
	}
}

func credentialsInstaller(home string) FileInstaller {
	return FileInstaller{
		HomeDirectory: home,
		Secrets: func() map[string]string {
			//nolint:gosec // A sentinel, which is non-secret by construction.
			return map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-sentinel"}
		},
	}
}

// What Claude Code writes on a 401: the same file, the credential fields
// emptied, everything else kept.
//
//nolint:gosec // Emptied fields, which is the whole point of the fixture.
const loggedOutCredentials = `{"claudeAiOauth":{"accessToken":"","refreshToken":"","expiresAt":0,"scopes":["user:inference"],"subscriptionType":"max"}}`

func TestRestoreSecretFilesRewritesClearedCredential(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	harness := credentialsHarness()
	ctx := context.Background()

	if err := installer.EnsureInstalled(ctx, harness, "", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, credentialsPath)
	if err := os.WriteFile(path, []byte(loggedOutCredentials), 0o600); err != nil {
		t.Fatal(err)
	}

	restored, err := installer.RestoreSecretFiles(ctx, harness, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 || restored[0] != credentialsPath {
		t.Fatalf("restored = %v, want [%s]", restored, credentialsPath)
	}

	var got struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.OAuth.AccessToken != "sk-ant-oat01-sentinel" {
		t.Fatalf("accessToken = %q, want the sentinel back", got.OAuth.AccessToken)
	}
	if got.OAuth.ExpiresAt != 4102444800000 {
		t.Fatalf("expiresAt = %d, want the far-future expiry back", got.OAuth.ExpiresAt)
	}
}

func TestRestoreSecretFilesLeavesAnIntactFileAlone(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	harness := credentialsHarness()
	ctx := context.Background()

	if err := installer.EnsureInstalled(ctx, harness, "", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, credentialsPath)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	restored, err := installer.RestoreSecretFiles(ctx, harness, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing rewritten", restored)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("file was rewritten despite still carrying its sentinel")
	}
}

// A harness rewriting a file it owns keeps its own content, as long as the
// sentinel it was delivered is still in there.
func TestRestoreSecretFilesKeepsHarnessEditsAroundTheSentinel(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	harness := credentialsHarness()
	ctx := context.Background()

	if err := installer.EnsureInstalled(ctx, harness, "", nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, credentialsPath)
	edited := `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-sentinel","subscriptionType":"pro"}}`
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := installer.RestoreSecretFiles(ctx, harness, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != edited {
		t.Fatalf("file = %s, want the harness's own edit preserved", raw)
	}
}

func TestRestoreSecretFilesIgnoresFilesWithoutASentinel(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	harness := config.Harness{
		ID: "harness_test",
		Files: []config.HarnessFile{
			{Path: ".claude/settings.json", Template: true, Content: `{"model":"opus"}`},
			// createOnly says the sandbox owns the file after the first write.
			{Path: ".claude/owned.json", Template: true, CreateOnly: true, Content: `{"t":"{{ .secrets.CLAUDE_CODE_OAUTH_TOKEN }}"}`},
		},
	}
	ctx := context.Background()

	if err := installer.EnsureInstalled(ctx, harness, "", nil); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".claude/settings.json", ".claude/owned.json"} {
		if err := os.WriteFile(filepath.Join(home, name), []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	restored, err := installer.RestoreSecretFiles(ctx, harness, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 0 {
		t.Fatalf("restored = %v, want nothing: one file carries no sentinel, the other is createOnly", restored)
	}
}

// A missing file is restored too: it is the same loss of the credential, and
// the harness reads the file's absence as being logged out either way.
func TestRestoreSecretFilesRecreatesADeletedFile(t *testing.T) {
	home := t.TempDir()
	installer := credentialsInstaller(home)
	harness := credentialsHarness()
	ctx := context.Background()

	if err := installer.EnsureInstalled(ctx, harness, "", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, credentialsPath)); err != nil {
		t.Fatal(err)
	}

	restored, err := installer.RestoreSecretFiles(ctx, harness, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored = %v, want the deleted file back", restored)
	}
	if _, err := os.Stat(filepath.Join(home, credentialsPath)); err != nil {
		t.Fatal(err)
	}
}

// countingInstaller records how many times the reconciler asked it to work.
type countingInstaller struct{ calls atomic.Int32 }

func (c *countingInstaller) EnsureInstalled(context.Context, config.Harness, string, map[string]string) error {
	return nil
}

func (c *countingInstaller) RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error) {
	c.calls.Add(1)
	return nil, nil
}

// A config sandbox's credentials file is the configure flow's output, not a
// delivery: the user logs in inside the session and the script reads the real
// credential back out of that file. Restoring a sentinel over it would
// overwrite the login being captured.
func TestWatchSecretFilesSkipsConfigSandboxes(t *testing.T) {
	installer := &countingInstaller{}
	svc := &Service{
		harness:     credentialsHarness(),
		installer:   installer,
		harnessMode: configHarnessMode,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		svc.WatchSecretFiles(ctx, slog.New(slog.DiscardHandler))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("WatchSecretFiles did not return for a config sandbox")
	}
	if n := installer.calls.Load(); n != 0 {
		t.Fatalf("restore calls = %d, want none in a config sandbox", n)
	}
}

// Outside config mode it reconciles immediately rather than waiting a tick.
func TestWatchSecretFilesReconcilesOnStart(t *testing.T) {
	installer := &countingInstaller{}
	svc := &Service{harness: credentialsHarness(), installer: installer}
	ctx, cancel := context.WithCancel(context.Background())
	go svc.WatchSecretFiles(ctx, slog.New(slog.DiscardHandler))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && installer.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if n := installer.calls.Load(); n == 0 {
		t.Fatal("WatchSecretFiles did not reconcile on start")
	}
}
