package sandboxcreate

import (
	"os"
	"path/filepath"
	"testing"

	apimodel "github.com/discobox-ai/discobox/api/model"
)

func findSecretInput(t *testing.T, secrets []apimodel.SandboxSecretInput, env string) apimodel.SandboxSecretInput {
	t.Helper()
	for _, s := range secrets {
		if s.Env == env {
			return s
		}
	}
	t.Fatalf("secret %q not found", env)
	return apimodel.SandboxSecretInput{}
}

func assertInlineSecret(t *testing.T, secrets []apimodel.SandboxSecretInput, env, want string) {
	t.Helper()
	s := findSecretInput(t, secrets, env)
	if s.Value != want {
		t.Fatalf("%s inline value = %q, want %q", env, s.Value, want)
	}
}

func TestEnvAndSecretsFromOptions(t *testing.T) {
	t.Setenv("SHELL_API_KEY", "from-shell")
	t.Setenv("PLAIN_HOME", "/home/x")

	env, secrets, err := EnvAndSecretsFromOptions(
		[]string{"FOO=bar", "MY_API_KEY=inline-key", "DB_PASSWORD!=literal-pass", "SHELL_API_KEY", "PLAIN_HOME"},
		[]string{"OPENAI_KEY=sk-inline", "GITHUB=ghp_x@api.github.com", "EMAIL=me@example.com@", "STAR=v@*"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if env["FOO"] != "bar" {
		t.Fatalf("FOO = %q", env["FOO"])
	}
	if env["DB_PASSWORD"] != "literal-pass" {
		t.Fatalf("forced-plain DB_PASSWORD = %q, want literal-pass", env["DB_PASSWORD"])
	}
	if env["PLAIN_HOME"] != "/home/x" {
		t.Fatalf("PLAIN_HOME = %q", env["PLAIN_HOME"])
	}
	if _, ok := env["MY_API_KEY"]; ok {
		t.Fatal("MY_API_KEY should be a secret, not plain env")
	}
	if _, ok := env["SHELL_API_KEY"]; ok {
		t.Fatal("SHELL_API_KEY name is sensitive; should be a secret")
	}

	assertInlineSecret(t, secrets, "MY_API_KEY", "inline-key")
	assertInlineSecret(t, secrets, "SHELL_API_KEY", "from-shell")
	assertInlineSecret(t, secrets, "OPENAI_KEY", "sk-inline")

	if openai := findSecretInput(t, secrets, "OPENAI_KEY"); openai.Host.Set {
		t.Fatalf("OPENAI_KEY host = %q, want none: sent anywhere", openai.Host.Value)
	}
	github := findSecretInput(t, secrets, "GITHUB")
	if github.Value != "ghp_x" || github.Host.Or("") != "api.github.com" {
		t.Fatalf("GITHUB = %q to %q, want ghp_x to api.github.com", github.Value, github.Host.Or(""))
	}
	if email := findSecretInput(t, secrets, "EMAIL"); email.Value != "me@example.com" || email.Host.Set {
		t.Fatalf("EMAIL = %q to %q, want me@example.com anywhere", email.Value, email.Host.Or(""))
	}
	if star := findSecretInput(t, secrets, "STAR"); star.Value != "v" || star.Host.Or("") != "*" {
		t.Fatalf("STAR = %q to %q, want v to *", star.Value, star.Host.Or(""))
	}
}

func TestEnvAndSecretsDuplicateRejected(t *testing.T) {
	if _, _, err := EnvAndSecretsFromOptions([]string{"API_KEY=a"}, []string{"API_KEY=b"}); err == nil {
		t.Fatal("expected duplicate error across --env and --secret")
	}
}

func TestEnvAndSecretsBadForms(t *testing.T) {
	if _, _, err := EnvAndSecretsFromOptions(nil, []string{"DISCOBOX_TEST_UNSET_SECRET"}); err == nil {
		t.Fatal("expected error for a --secret read from a variable this shell does not set")
	}
	if _, _, err := EnvAndSecretsFromOptions(nil, []string{"EMPTY="}); err == nil {
		t.Fatal("expected error for an empty --secret value")
	}
	if _, _, err := EnvAndSecretsFromOptions(nil, []string{"MISSING=@/nonexistent/discobox-secret"}); err == nil {
		t.Fatal("expected error for a --secret file that does not exist")
	}
	if _, _, err := EnvAndSecretsFromOptions(nil, []string{"DB_PASSWORD=p@ss"}); err == nil {
		t.Fatal("expected error for a value whose @ is followed by no host")
	}
	if _, _, err := EnvAndSecretsFromOptions([]string{"=novalue"}, nil); err == nil {
		t.Fatal("expected error for --env with empty key")
	}
}

// A secret is best read from this shell or a file, so its value never sits in
// the command line, shell history or the process list. An @ straight after =
// is always a file, since a host cannot follow an empty value; the host is
// still what follows the last @.
func TestSecretsFromTheShellAndFromFiles(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_from_shell")
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("npm_from_file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	kube := filepath.Join(dir, "kube@config")
	if err := os.WriteFile(kube, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if err := os.WriteFile(filepath.Join(home, "pat"), []byte("from-home"), 0o600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"GH_TOKEN",
		"GH_COPY@api.github.com",
		"NPM_TOKEN=@" + token,
		"KUBE=@" + kube + "@10.0.0.5.nip.io",
		"PAT=@~/pat",
	}
	if _, _, err := EnvAndSecretsFromOptions(nil, args); err == nil {
		t.Fatal("GH_COPY is not set in this shell; want a refusal")
	}
	t.Setenv("GH_COPY", "ghp_copy")
	_, secrets, err := EnvAndSecretsFromOptions(nil, args)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, tc := range []struct{ env, value, host string }{
		{"GH_TOKEN", "ghp_from_shell", ""},
		{"GH_COPY", "ghp_copy", "api.github.com"},
		{"NPM_TOKEN", "npm_from_file", ""},                          // one trailing newline dropped
		{"KUBE", "apiVersion: v1\nkind: Config", "10.0.0.5.nip.io"}, // a path holding an @, then a host
		{"PAT", "from-home", ""},                                    // ~ is this user's home
	} {
		got := findSecretInput(t, secrets, tc.env)
		if got.Value != tc.value || got.Host.Or("") != tc.host {
			t.Fatalf("%s = %q to %q, want %q to %q", tc.env, got.Value, got.Host.Or(""), tc.value, tc.host)
		}
	}
}
