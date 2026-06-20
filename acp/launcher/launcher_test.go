package launcher

import (
	"os"
	"testing"

	"github.com/obot-platform/discobox/acp/registry"
)

func TestResolveUsesNPX(t *testing.T) {
	agent := registry.Agent{
		ID: "codex-acp",
		Distribution: registry.Distribution{NPX: &registry.PackageTarget{
			Package: "@zed-industries/codex-acp@0.16.0",
			Args:    []string{"--flag"},
			Env:     map[string]string{"A": "B"},
		}},
	}
	cmd, err := Resolve(agent)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Method != "npx" {
		t.Fatalf("method = %q, want npx", cmd.Method)
	}
	if cmd.Command != "npx" {
		t.Fatalf("command = %q, want npx", cmd.Command)
	}
	wantArgs := []string{"--yes", "@zed-industries/codex-acp@0.16.0", "--flag"}
	if len(cmd.Args) != len(wantArgs) {
		t.Fatalf("args = %#v, want %#v", cmd.Args, wantArgs)
	}
	for i := range wantArgs {
		if cmd.Args[i] != wantArgs[i] {
			t.Fatalf("args = %#v, want %#v", cmd.Args, wantArgs)
		}
	}
}

func TestResolveUsesUVX(t *testing.T) {
	agent := registry.Agent{
		ID: "example",
		Distribution: registry.Distribution{UVX: &registry.PackageTarget{
			Package: "example-acp==1.0.0",
			Args:    []string{"serve"},
		}},
	}
	cmd, err := Resolve(agent)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Method != "uvx" || cmd.Command != "uvx" {
		t.Fatalf("got %#v, want uvx command", cmd)
	}
	wantArgs := []string{"example-acp==1.0.0", "serve"}
	for i := range wantArgs {
		if cmd.Args[i] != wantArgs[i] {
			t.Fatalf("args = %#v, want %#v", cmd.Args, wantArgs)
		}
	}
}

func TestExecCommandInheritsEnvironment(t *testing.T) {
	t.Setenv("DISCOBOX_ACP_TEST_ENV", "base")
	cmd := ExecCommand(t.Context(), Command{Command: "echo", Env: map[string]string{"EXTRA": "1"}})
	if !containsEnv(cmd.Env, "DISCOBOX_ACP_TEST_ENV=base") {
		t.Fatalf("expected inherited env in %#v", cmd.Env)
	}
	if !containsEnv(cmd.Env, "EXTRA=1") {
		t.Fatalf("expected registry env in %#v", cmd.Env)
	}
}

func containsEnv(env []string, want string) bool {
	for _, got := range env {
		if got == want {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
