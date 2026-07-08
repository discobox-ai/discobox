package cli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestProjectFlagCompletionListsDefaultAndProjects(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects": `{"projects":[{"id":"project-1","ownerUserId":"user-1","name":"Project One","slug":"one","default":true,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]}`,
	})
	cmd := NewRootCommand()
	setFlag(t, cmd, "server", server.URL)

	completions, directive := flagCompletions(t, cmd, "project", "")

	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no file completion", directive)
	}
	assertCompletionValues(t, completions, "default", "project-1", "one")
}

func TestSandboxPositionalCompletionListsSandboxes(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/sandboxes": `{"sandboxes":[{"id":"sandbox-1","projectId":"project-1","createdByUserId":"user-1","config":{"name":"Alpha","image":"","cpuVcpus":0,"memoryBytes":0,"storageBytes":0},"runtime":{"phase":"running","desiredState":"running","lastOperationStatus":"success","generation":1,"observedGeneration":1,"restartGeneration":0,"restartedGeneration":0},"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]}`,
	})
	root := NewRootCommand()
	setFlag(t, root, "server", server.URL)
	setFlag(t, root, "project", "project-1")
	cmd := findCommand(t, root, "sandbox", "get")

	completions, directive := cmd.ValidArgsFunction(cmd, nil, "sand")

	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no file completion", directive)
	}
	assertCompletionValues(t, completions, "sandbox-1")
}

func TestProviderFlagCompletionListsProviders(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/projects/project-1/providers": `{"providers":[{"id":"provider-1","projectId":"project-1","type":"docker","name":"Docker","builtIn":false,"disabled":false,"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}]}`,
	})
	root := NewRootCommand()
	setFlag(t, root, "server", server.URL)
	setFlag(t, root, "project", "project-1")
	cmd := findCommand(t, root, "sandbox", "create")

	completions, directive := flagCompletions(t, cmd, "provider-instance", "")

	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no file completion", directive)
	}
	assertCompletionValues(t, completions, "provider-1")
}

func TestTerminalCompletionUsesSandboxScope(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/api/projects/project-1/sandboxes/sandbox-1/execs": `{"execs":[{"id":"terminal-1","agentId":"codex","status":"running","command":["codex"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}]}`,
	})
	root := NewRootCommand()
	setFlag(t, root, "server", server.URL)
	setFlag(t, root, "project", "project-1")
	parent := findCommand(t, root, "terminal")
	setFlag(t, parent, "sandbox-id", "sandbox-1")
	cmd := findCommand(t, root, "terminal", "logs")

	completions, directive := cmd.ValidArgsFunction(cmd, nil, "")

	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no file completion", directive)
	}
	assertCompletionValues(t, completions, "terminal-1")
}

func TestExecCompletionUsesSandboxScope(t *testing.T) {
	server := completionServer(t, map[string]string{
		"/api/projects/project-1/sandboxes/sandbox-1/execs": `{"execs":[{"id":"exec-1","status":"exited","command":["go","test"],"workdir":"/workspace","tty":false,"createdAt":"2026-01-01T00:00:00Z"}]}`,
	})
	root := NewRootCommand()
	setFlag(t, root, "server", server.URL)
	setFlag(t, root, "project", "project-1")
	parent := findCommand(t, root, "exec")
	setFlag(t, parent, "sandbox-id", "sandbox-1")
	cmd := findCommand(t, root, "exec", "logs")

	completions, directive := cmd.ValidArgsFunction(cmd, nil, "")

	if directive&cobra.ShellCompDirectiveNoFileComp == 0 {
		t.Fatalf("directive = %v, want no file completion", directive)
	}
	assertCompletionValues(t, completions, "exec-1")
}

func completionServer(t *testing.T, responses map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := responses[r.URL.Path]
		if !ok {
			t.Fatalf("unexpected completion path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

func findCommand(t *testing.T, root *cobra.Command, args ...string) *cobra.Command {
	t.Helper()
	cmd, _, err := root.Find(args)
	if err != nil {
		t.Fatalf("find command %v: %v", args, err)
	}
	if cmd == nil {
		t.Fatalf("find command %v returned nil", args)
	}
	return cmd
}

func setFlag(t *testing.T, cmd *cobra.Command, name, value string) {
	t.Helper()
	if err := cmd.Flags().Set(name, value); err != nil {
		if err := cmd.PersistentFlags().Set(name, value); err != nil {
			t.Fatalf("set flag %s on %s: %v", name, cmd.CommandPath(), err)
		}
	}
}

func flagCompletions(t *testing.T, cmd *cobra.Command, flag, toComplete string) ([]string, cobra.ShellCompDirective) {
	t.Helper()
	fn, ok := cmd.GetFlagCompletionFunc(flag)
	if !ok {
		t.Fatalf("flag %s on %s has no completion function", flag, cmd.CommandPath())
	}
	return fn(cmd, nil, toComplete)
}

func assertCompletionValues(t *testing.T, completions []string, want ...string) {
	t.Helper()
	got := map[string]bool{}
	for _, completion := range completions {
		value, _, _ := strings.Cut(completion, "\t")
		got[value] = true
	}
	for _, value := range want {
		if !got[value] {
			t.Fatalf("completions = %v, want value %q", completions, value)
		}
	}
}
