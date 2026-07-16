package sandboxcreate

import (
	"testing"

	apimodel "github.com/obot-platform/discobox/api/model"
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
	if !s.Value.Set || s.Value.Value != want {
		t.Fatalf("%s inline value = %q set=%v, want %q", env, s.Value.Value, s.Value.Set, want)
	}
	if s.SecretId.Set {
		t.Fatalf("%s inline value must not set secretId", env)
	}
}

func TestEnvAndSecretsFromOptions(t *testing.T) {
	t.Setenv("SHELL_API_KEY", "from-shell")
	t.Setenv("PLAIN_HOME", "/home/x")

	env, secrets, err := EnvAndSecretsFromOptions(
		[]string{"FOO=bar", "MY_API_KEY=inline-key", "DB_PASSWORD!=literal-pass", "SHELL_API_KEY", "PLAIN_HOME"},
		[]string{"OPENAI_KEY=sk-inline", "GITHUB=<sec_123>"},
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

	github := findSecretInput(t, secrets, "GITHUB")
	if !github.SecretId.Set || github.SecretId.Value != "sec_123" {
		t.Fatalf("GITHUB reference = %q set=%v, want sec_123", github.SecretId.Value, github.SecretId.Set)
	}
	if github.Value.Set {
		t.Fatal("GITHUB reference must not set an inline value")
	}
}

func TestEnvAndSecretsDuplicateRejected(t *testing.T) {
	if _, _, err := EnvAndSecretsFromOptions([]string{"API_KEY=a"}, []string{"API_KEY=b"}); err == nil {
		t.Fatal("expected duplicate error across --env and --secret")
	}
}

func TestEnvAndSecretsBadForms(t *testing.T) {
	if _, _, err := EnvAndSecretsFromOptions(nil, []string{"NOEQUALS"}); err == nil {
		t.Fatal("expected error for --secret without =")
	}
	if _, _, err := EnvAndSecretsFromOptions([]string{"=novalue"}, nil); err == nil {
		t.Fatal("expected error for --env with empty key")
	}
}
