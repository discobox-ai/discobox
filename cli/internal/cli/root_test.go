package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func TestWriteProviderTableIncludesConfig(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeProvider(cmd, &apimodel.SandboxProviderInstance{
		ID:       "provider-1",
		Name:     "Docker",
		Type:     "docker",
		Config:   jx.Raw(`{"socketPath":"/var/run/docker.sock","token":"do-secret","nested":{"apiKey":"api-secret"},"items":[{"password":"password-secret"}]}`),
		Disabled: true,
	})
	if err != nil {
		t.Fatalf("writeProvider: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"FIELD",
		"CONFIG",
		`"socketPath":"/var/run/docker.sock"`,
		`"token":"[REDACTED]"`,
		`"apiKey":"[REDACTED]"`,
		`"password":"[REDACTED]"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("provider output = %q, want %q", output, want)
		}
	}
	for _, leaked := range []string{"do-secret", "api-secret", "password-secret", "bootstrap-secret"} {
		if strings.Contains(output, leaked) {
			t.Fatalf("provider output leaked secret %q: %q", leaked, output)
		}
	}
}

func TestWritePoolTableIncludesRuntimeState(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writePool(cmd, &apimodel.Pool{
		ID:                 "pool-1",
		ProviderInstanceId: "provider-1",
		State:              "failed",
		ErrorMessage:       apiclientgen.NewOptString("docker create failed"),
		Name:               "Default",
	})
	if err != nil {
		t.Fatalf("writePool: %v", err)
	}

	output := out.String()
	for _, want := range []string{
		"STATE",
		"failed",
		"MESSAGE",
		"docker create failed",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("pool output = %q, want %q", output, want)
		}
	}
}

func TestWritePoolsTableIncludesStateAndMessage(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writePools(cmd, []apimodel.Pool{{
		ID:                 "pool-1",
		Name:               "Default",
		ProviderInstanceId: "provider-1",
		State:              "registering",
		ErrorMessage:       apiclientgen.NewOptString("pool agent did not register before timeout"),
	}})
	if err != nil {
		t.Fatalf("writePools: %v", err)
	}

	output := out.String()
	for _, want := range []string{"STATE", "registering", "pool agent did not register before timeout"} {
		if !strings.Contains(output, want) {
			t.Fatalf("pools output = %q, want %q", output, want)
		}
	}
}

func TestQuietListWritersPrintFullIDsOnly(t *testing.T) {
	app := &App{output: "table", quiet: true}
	tests := []struct {
		name  string
		write func(*cobra.Command) error
		want  string
	}{
		{
			name: "sandboxes",
			write: func(cmd *cobra.Command) error {
				return app.writeSandboxes(cmd, []apimodel.Sandbox{{ID: "sandbox-full-id"}}, false)
			},
			want: "sandbox-full-id\n",
		},
		{
			name: "provider catalog",
			write: func(cmd *cobra.Command) error {
				return app.writeProviderCatalog(cmd, []apimodel.SandboxProviderCatalogItem{{ID: "docker"}})
			},
			want: "docker\n",
		},
		{
			name: "providers",
			write: func(cmd *cobra.Command) error {
				return app.writeProviders(cmd, []apimodel.SandboxProviderInstance{{ID: "provider-full-id"}})
			},
			want: "provider-full-id\n",
		},
		{
			name: "pools",
			write: func(cmd *cobra.Command) error {
				return app.writePools(cmd, []apimodel.Pool{{ID: "pool-full-id"}})
			},
			want: "pool-full-id\n",
		},
		{
			name: "harnesses",
			write: func(cmd *cobra.Command) error {
				return app.writeHarnesses(cmd, []apimodel.HarnessConfig{{ID: "harness-full-id"}})
			},
			want: "harness-full-id\n",
		},
		{
			name: "jobs",
			write: func(cmd *cobra.Command) error {
				return app.writeJobs(cmd, []apimodel.Job{{ID: "job-full-id"}})
			},
			want: "job-full-id\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			var out bytes.Buffer
			cmd.SetOut(&out)
			if err := tt.write(cmd); err != nil {
				t.Fatalf("write quiet output: %v", err)
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("quiet output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRootCommandHelp(t *testing.T) {
	cmd := NewRootCommand()
	if cmd.Name() != "discobox" {
		t.Fatalf("root command name = %q, want discobox", cmd.Name())
	}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute help: %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("\n  run ")) {
		t.Fatalf("help output = %q, want visible run command", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("\n  admin ")) {
		t.Fatalf("help output = %q, want visible admin command", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("Manage advanced Discobox configuration")) {
		t.Fatalf("help output = %q, want admin command description", out.String())
	}
	// shell is the exception among the admin children: the friendly root `shell`
	// runs a command in a sandbox, while `admin exec` keeps the raw subcommands.
	if command, _, err := cmd.Find([]string{"shell"}); err != nil || command.Name() != "shell" {
		t.Fatalf("find root shell: command=%v err=%v", command, err)
	}
	for _, unavailableAtRoot := range []string{"box", "terminal", "exec", "provider", "job", "harnesses", "hooks", "server"} {
		command, _, err := cmd.Find([]string{unavailableAtRoot})
		if err == nil && command.Name() == unavailableAtRoot {
			t.Fatalf("root command still exposes %q", unavailableAtRoot)
		}
	}
	for _, child := range []string{"box", "terminal", "exec", "provider", "job", "harnesses", "hooks", "server"} {
		command, args, err := cmd.Find([]string{"admin", child})
		if err != nil || len(args) != 0 || command.Name() != child {
			t.Fatalf("find admin child %q: command=%v args=%v err=%v", child, command, args, err)
		}
	}
	if command, _, err := cmd.Find([]string{"debug"}); err == nil && command.Name() == "debug" {
		t.Fatal("root command still exposes renamed debug command")
	}
}

func TestRootCommandDisablesCommandSorting(t *testing.T) {
	previous := cobra.EnableCommandSorting
	t.Cleanup(func() {
		cobra.EnableCommandSorting = previous
	})

	cobra.EnableCommandSorting = true
	NewRootCommand()

	if cobra.EnableCommandSorting {
		t.Fatal("Cobra command sorting is enabled")
	}
}

func TestProjectIDDefaultsToDefaultAlias(t *testing.T) {
	app := &App{projectID: defaultProjectAlias}

	projectID, err := app.projectIDValue()
	if err != nil {
		t.Fatalf("projectIDValue: %v", err)
	}
	if projectID != "default" {
		t.Fatalf("projectID = %s", projectID)
	}
}

func TestRootCommandRejectsInvalidOutputFormat(t *testing.T) {
	cmd := NewRootCommand()
	cmd.SetArgs([]string{"--output", "yaml", "admin", "box", "ls"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("execute error = nil, want invalid output error")
	}
}

func TestListSubcommandsUseLSWithListAlias(t *testing.T) {
	root := NewRootCommand()
	paths := [][]string{
		{"admin", "box", "ls"},
		{"admin", "terminal", "ls"},
		{"admin", "exec", "ls"},
		{"admin", "pool", "ls"},
		{"admin", "provider", "ls"},
		{"admin", "job", "ls"},
		{"secret", "ls"},
		{"secret", "grant", "ls"},
		{"secret", "request", "ls"},
		{"admin", "harnesses", "ls"},
		{"admin", "harnesses", "secrets", "ls"},
	}

	for _, path := range paths {
		cmd, args, err := root.Find(path)
		if err != nil {
			t.Fatalf("find %v: %v", path, err)
		}
		if len(args) != 0 {
			t.Fatalf("find %v returned args %v", path, args)
		}
		if cmd.Name() != "ls" {
			t.Fatalf("find %v returned command %q, want ls", path, cmd.Name())
		}

		aliasPath := append(append([]string(nil), path[:len(path)-1]...), "list")
		aliasCmd, aliasArgs, err := root.Find(aliasPath)
		if err != nil {
			t.Fatalf("find alias %v: %v", aliasPath, err)
		}
		if len(aliasArgs) != 0 {
			t.Fatalf("find alias %v returned args %v", aliasPath, aliasArgs)
		}
		if aliasCmd != cmd {
			t.Fatalf("find alias %v returned %q, want %q", aliasPath, aliasCmd.CommandPath(), cmd.CommandPath())
		}
	}
}

func TestSandboxListQuietCommandPrintsFullIDsOnly(t *testing.T) {
	const sandboxID = "sbx_9qk5n25t2hh2rv00"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/sandboxes" {
			t.Fatalf("path = %q, want project sandboxes path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sandboxes":[` + testSandboxJSON(sandboxID, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z") + `]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "box", "list", "-q"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute sandbox list -q: %v", err)
	}
	if got := out.String(); got != sandboxID+"\n" {
		t.Fatalf("quiet output = %q, want full sandbox ID only", got)
	}
}

func TestTerminalListUsesAdminCommand(t *testing.T) {
	var requested bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		if got := r.URL.Path; got != "/api/projects/project-1/sandboxes/sandbox-1/execs" {
			t.Fatalf("path = %q, want sandbox exec path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"execs":[]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal", "--discobox-id", "sandbox-1", "ls"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal list: %v", err)
	}
	if !requested {
		t.Fatal("expected terminal list request")
	}
	if output := out.String(); !strings.Contains(output, "ID") || !strings.Contains(output, "AGENT") || !strings.Contains(output, "STATUS") {
		t.Fatalf("terminal list output = %q, want table header", output)
	}
}

func TestTerminalCreateFallsBackWhenStartResponseIsTruncated(t *testing.T) {
	const terminalID = "terminal-full-id"
	var created, started, listed bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/execs":
			created = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"exec":{"id":"` + terminalID + `","status":"starting","command":["/bin/bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/execs/"+terminalID+"/start":
			started = true
			_, _ = w.Write([]byte(`{`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/execs/"+terminalID:
			listed = true
			_, _ = w.Write([]byte(`{"id":"` + terminalID + `","status":"running","command":["/bin/bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal", "--discobox-id", "sandbox-1", "create"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal create: %v", err)
	}
	if !created || !started || !listed {
		t.Fatalf("requests created=%v started=%v listed=%v, want all true", created, started, listed)
	}
	output := out.String()
	if !strings.Contains(output, terminalID) || !strings.Contains(output, "running") {
		t.Fatalf("terminal create output = %q, want fallback terminal row", output)
	}
}

// "primary" is the sandbox-agent's virtual exec id: it must reach the agent
// verbatim, without being matched against the exec listing as a short id, and
// the agent's own rejection is what the user sees.
func TestTerminalAttachPrimaryUsesVirtualExecID(t *testing.T) {
	var attachPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/attach") {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		attachPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal", "--discobox-id", "sandbox-1", "attach", "primary"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute terminal attach primary error = nil")
	}
	if want := "/api/projects/project-1/sandboxes/sandbox-1/execs/primary/attach"; attachPath != want {
		t.Fatalf("attach path = %q, want %q", attachPath, want)
	}
	if got := err.Error(); !strings.Contains(got, "has ended with exit code 0") {
		t.Fatalf("execute terminal attach primary error = %q", got)
	}
}

// `discobox attach` is the root-command shortcut for `admin terminal attach
// primary --sandbox-id`: it must reach the same virtual primary exec, with no
// other behavior in between.
func TestAttachUsesVirtualPrimaryExecID(t *testing.T) {
	var attachPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/attach") {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		attachPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sandbox exec ex_1 has ended with exit code 0 and its session is no longer available to attach"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "attach", "sandbox-1"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute attach error = nil")
	}
	if want := "/api/projects/project-1/sandboxes/sandbox-1/execs/primary/attach"; attachPath != want {
		t.Fatalf("attach path = %q, want %q", attachPath, want)
	}
	if got := err.Error(); !strings.Contains(got, "has ended with exit code 0") {
		t.Fatalf("execute attach error = %q", got)
	}
}

func TestTerminalCreateTextPlainErrorIncludesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/projects/project-1/sandboxes/sandbox-1/execs" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("sandbox worker is not assigned\n"))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal", "--discobox-id", "sandbox-1", "create"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("execute terminal create error = nil")
	}
	if got := err.Error(); !strings.Contains(got, "409") || !strings.Contains(got, "sandbox worker is not assigned") {
		t.Fatalf("execute terminal create error = %q", got)
	}
}

func TestTerminalCreateEnvSupportsShortFlagAndShellLookup(t *testing.T) {
	const terminalID = "terminal-full-id"
	t.Setenv("SHELL_ENV_VALUE", "from-shell")
	var env map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/execs":
			var body struct {
				Env map[string]string `json:"env"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create body: %v", err)
			}
			env = body.Env
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"exec":{"id":"` + terminalID + `","status":"starting","command":["/bin/bash"],"workdir":"/workspace","tty":true,"createdAt":"2026-01-01T00:00:00Z"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/projects/project-1/sandboxes/sandbox-1/execs/"+terminalID+"/start":
			_, _ = w.Write([]byte(`{"id":"` + terminalID + `","status":"running","command":["/bin/bash"],"workdir":"/workspace","createdAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "terminal", "--discobox-id", "sandbox-1", "create", "-e", "EXPLICIT=value", "-e", "SHELL_ENV_VALUE"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute terminal create: %v", err)
	}
	if got := env["EXPLICIT"]; got != "value" {
		t.Fatalf("EXPLICIT env = %q, want value", got)
	}
	if got := env["SHELL_ENV_VALUE"]; got != "from-shell" {
		t.Fatalf("SHELL_ENV_VALUE env = %q, want from-shell", got)
	}
}

func TestHarnessSetDefaultCommand(t *testing.T) {
	const harnessID = "harness-full-id"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/harness-configs":
			// Selector resolution lists configs to match a slug or name first; an
			// unrecognized value like a full ID falls through unchanged.
			_, _ = w.Write([]byte(`{"harnessConfigs":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/projects/project-1/harness-configs/"+harnessID+"/default":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"welcomed":true,"defaultHarnessConfigId":"` + harnessID + `","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("request = %s %s, want list then PUT set-default path", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "harnesses", "set-default", harnessID})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute set-default: %v", err)
	}
	if got, want := out.String(), "default harness config set to "+harnessID+"\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestSecretCreateCommandSendsSecretValue(t *testing.T) {
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/secrets" {
			t.Fatalf("request = %s %s, want POST create secret path", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"secret-1","projectId":"project-1","name":"github","type":"token","host":"github.com","maxGrantTTLSeconds":7200,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "create", "--name", "github", "--type", "token", "--host", "github.com", "--max-grant-ttl", "7200", "--token", "token-value"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute secret create: %v", err)
	}
	if posted["name"] != "github" || posted["type"] != "token" || posted["host"] != "github.com" {
		t.Fatalf("posted body = %#v", posted)
	}
	// The limit travels under the name that says what it is: the longest a
	// grant on this credential may live, not a default anything may exceed.
	if posted["maxGrantTTLSeconds"] != float64(7200) {
		t.Fatalf("posted grant limit = %#v, want 7200 under maxGrantTTLSeconds", posted["maxGrantTTLSeconds"])
	}
	value, ok := posted["value"].(map[string]any)
	if !ok || value["token"] != "token-value" {
		t.Fatalf("posted value = %#v, want token", posted["value"])
	}
	if output := out.String(); !strings.Contains(output, "github") || strings.Contains(output, "token-value") {
		t.Fatalf("secret output = %q, want metadata without secret value", output)
	}
}

func TestSecretRequestApproveCommandSendsSelectedSecretID(t *testing.T) {
	const (
		requestID = "request-1"
		secretID  = "secret-1"
	)
	var approved map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/projects/project-1/secret-requests/"+requestID+"/approve":
			if err := json.NewDecoder(r.Body).Decode(&approved); err != nil {
				t.Fatalf("decode approve body: %v", err)
			}
			_, _ = w.Write([]byte(`{"id":"` + requestID + `","projectId":"project-1","requestedBy":"user-1","type":"token","status":"approved","secretId":"` + secretID + `","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/secrets":
			_, _ = w.Write([]byte(`{"secrets":[{"id":"` + secretID + `","projectId":"project-1","name":"selected","type":"token","maxGrantTTLSeconds":3600,"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
		default:
			t.Fatalf("unexpected request = %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "secret", "request", "approve", requestID, "--secret-id", secretID, "--grant-ttl", "600"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute secret request approve: %v", err)
	}
	if approved["secretId"] != secretID || approved["grantTTLSeconds"] != float64(600) {
		t.Fatalf("approve body = %#v", approved)
	}
	if output := out.String(); !strings.Contains(output, requestID) || !strings.Contains(output, "approved") {
		t.Fatalf("approve output = %q, want approved request", output)
	}
}

func TestHarnessListShowsProjectDefault(t *testing.T) {
	const defaultHarnessID = "harness-default-full-id"
	requested := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested[r.Method+" "+r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1/harness-configs":
			_, _ = w.Write([]byte(`{"harnessConfigs":[` +
				`{"id":"harness-other-full-id","projectId":"project-1","slug":"other","name":"Other","builtIn":false,"configured":false,"runCommand":["other"],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"},` +
				`{"id":"` + defaultHarnessID + `","projectId":"project-1","slug":"codex","name":"Codex","builtIn":true,"configured":true,"runCommand":["codex"],"createdAt":"2026-01-01T00:01:00Z","updatedAt":"2026-01-01T00:01:00Z"}` +
				`]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/projects/project-1":
			_, _ = w.Write([]byte(`{"id":"project-1","ownerUserId":"user-1","name":"Project","default":true,"welcomed":true,"defaultHarnessConfigId":"` + defaultHarnessID + `","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "harnesses", "ls"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute harnesses list: %v", err)
	}
	if requested[http.MethodGet+" /projects/project-1"] != 1 {
		t.Fatalf("project requests = %d, want 1", requested[http.MethodGet+" /projects/project-1"])
	}
	output := out.String()
	if !strings.Contains(output, "DEFAULT") {
		t.Fatalf("output = %q, want DEFAULT column", output)
	}
	if !strings.Contains(output, defaultHarnessID+"  codex  Codex  yes") {
		t.Fatalf("output = %q, want default harness marked yes", output)
	}
	if strings.Contains(output, "Other  yes") {
		t.Fatalf("output = %q, non-default harness marked default", output)
	}
}

func TestParseHarnessFileFlagsInlineContent(t *testing.T) {
	files, err := parseHarnessFileFlags([]string{`.claude/settings.json={"theme":"dark"}`}, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 || files[0].Path != ".claude/settings.json" || files[0].Content != `{"theme":"dark"}` {
		t.Fatalf("files = %#v", files)
	}
}

func TestParseHarnessFileFlagsLocalFileContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(`{"theme":"dark"}`), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	files, err := parseHarnessFileFlags([]string{".claude/settings.json=@" + path}, nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 || files[0].Path != ".claude/settings.json" || files[0].Content != `{"theme":"dark"}` {
		t.Fatalf("files = %#v", files)
	}
}

func TestParseHarnessFileFlagsRejectsMissingEquals(t *testing.T) {
	if _, err := parseHarnessFileFlags([]string{"no-equals-sign"}, nil); err == nil {
		t.Fatalf("expected error for missing '='")
	}
}

func TestParseHarnessFileFlagsWithCreateOnlyPaths(t *testing.T) {
	files, err := parseHarnessFileFlags(
		[]string{`.claude/settings.json={"theme":"dark"}`, ".github/config=ok"},
		[]string{".claude/settings.json"},
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %#v", files)
	}
	if files[0].Path != ".claude/settings.json" || !files[0].CreateOnly.Or(false) {
		t.Fatalf("files = %#v", files)
	}
	if files[1].Path != ".github/config" || files[1].CreateOnly.Or(false) {
		t.Fatalf("files = %#v", files)
	}
}

func TestParseHarnessFileFlagsRejectsMissingCreateOnlyMatch(t *testing.T) {
	if _, err := parseHarnessFileFlags([]string{".claude/settings.json={\"theme\":\"dark\"}"}, []string{".does-not-exist"}); err == nil {
		t.Fatalf("expected error for missing --file match")
	}
}

func TestHarnessCreateSendsCreateOnlyFileFlag(t *testing.T) {
	const harnessID = "harness-full-id"
	var gotFiles []apimodel.HarnessConfigFile
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/harness-configs" {
			t.Fatalf("request = %s %s, want create harness config path", r.Method, r.URL.Path)
		}
		var body struct {
			Files []apimodel.HarnessConfigFile `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode create body: %v", err)
		}
		gotFiles = body.Files
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"custom","name":"Custom","builtIn":false,"configured":false,"runCommand":["claude"],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--server", server.URL, "--project", "project-1", "admin", "harnesses", "create",
		"--name", "Custom", "--image", "example.com/custom:latest",
		"--file", `.claude/settings.json={"theme":"dark"}`,
		"--create-only-file", ".claude/settings.json",
		"--file", ".github/config=ok",
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute harnesses create: %v", err)
	}

	claudeIndex := -1
	githubIndex := -1
	for i, file := range gotFiles {
		switch file.Path {
		case ".claude/settings.json":
			claudeIndex = i
		case ".github/config":
			githubIndex = i
		}
	}
	if claudeIndex < 0 || githubIndex < 0 {
		t.Fatalf("files sent to server = %#v", gotFiles)
	}
	if !gotFiles[claudeIndex].CreateOnly.Or(false) || gotFiles[githubIndex].CreateOnly.Or(false) {
		t.Fatalf("files sent to server = %#v", gotFiles)
	}
}

func TestHarnessCreateSendsFilesFlag(t *testing.T) {
	const harnessID = "harness-full-id"
	var gotFiles []apimodel.HarnessConfigFile
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/harness-configs" {
			t.Fatalf("request = %s %s, want create harness config path", r.Method, r.URL.Path)
		}
		var body struct {
			Files []apimodel.HarnessConfigFile `json:"files"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode create body: %v", err)
		}
		gotFiles = body.Files
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + harnessID + `","projectId":"project-1","slug":"custom","name":"Custom","builtIn":false,"configured":false,"runCommand":["claude"],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{
		"--server", server.URL, "--project", "project-1", "admin", "harnesses", "create",
		"--name", "Custom", "--image", "example.com/custom:latest",
		"--file", `.claude/settings.json={"theme":"dark"}`,
	})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute harnesses create: %v", err)
	}
	if len(gotFiles) != 1 || gotFiles[0].Path != ".claude/settings.json" || gotFiles[0].Content != `{"theme":"dark"}` {
		t.Fatalf("files sent to server = %#v", gotFiles)
	}
}

func TestWriteSandboxesShowFolderColumn(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	sandboxes := []apimodel.Sandbox{
		{
			ID:        "sandbox-1",
			Config:    apimodel.SandboxConfig{Name: "alpha"},
			CreatedAt: time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC),
			Origin:    apiclientgen.NewOptOrigin(apiclientgen.Origin{ProjectPath: "/home/darren/src/disco2"}),
		},
		{
			ID:        "sandbox-2",
			Config:    apimodel.SandboxConfig{Name: "beta"},
			CreatedAt: time.Date(2026, 6, 17, 0, 0, 1, 0, time.UTC),
		},
	}

	if err := app.writeSandboxes(cmd, sandboxes, true); err != nil {
		t.Fatalf("writeSandboxes: %v", err)
	}
	output := out.String()
	for _, want := range []string{"FOLDER", "/home/darren/src/disco2"} {
		if !strings.Contains(output, want) {
			t.Fatalf("sandboxes output = %q, want %q", output, want)
		}
	}

	out.Reset()
	if err := app.writeSandboxes(cmd, sandboxes, false); err != nil {
		t.Fatalf("writeSandboxes: %v", err)
	}
	if strings.Contains(out.String(), "FOLDER") {
		t.Fatalf("sandboxes output = %q, did not want FOLDER column", out.String())
	}
}

func TestWriteSandboxesTableIncludesErrorMessage(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	err := app.writeSandboxes(cmd, []apimodel.Sandbox{{
		ID:        "sandbox-1",
		Config:    apimodel.SandboxConfig{Name: "alpha"},
		CreatedAt: time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 17, 0, 0, 1, 0, time.UTC),
		Runtime: apimodel.SandboxRuntime{
			State:        "failed",
			DesiredState: "running",
			DisplayState: apiclientgen.NewOptSandboxRuntimeDisplayState("error"),
			ErrorMessage: apiclientgen.NewOptString("worker-agent request failed: git clone failed"),
			Generation:   1,
		},
	}}, false)
	if err != nil {
		t.Fatalf("writeSandboxes: %v", err)
	}

	output := out.String()
	for _, want := range []string{"STATE", "error", "ERROR", "worker-agent request failed: git clone failed"} {
		if !strings.Contains(output, want) {
			t.Fatalf("sandboxes output = %q, want %q", output, want)
		}
	}
	for _, unwanted := range []string{"DESIRED", "GENERATION", "OBSERVED"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("sandboxes output = %q, did not want column %q", output, unwanted)
		}
	}
}

func TestJobsCommandListsProjectJobs(t *testing.T) {
	const jobID = "job_9qk5n25t2hh2rv00"
	const resourceID = "sbx_hqnk550g3821ck00"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs" {
			t.Fatalf("path = %q, want project jobs path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":"` + jobID + `","type":"sandbox.reconcile","status":"failed","attempts":1,"error":"failed to launch sandbox because the worker container exited before it could register with the control plane","resourceType":"sandbox","resourceId":"` + resourceID + `","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "jobs"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute jobs: %v", err)
	}
	output := out.String()
	for _, want := range []string{"ID", "CREATED", "ERROR", jobID, "sandbox.reconcile", "failed", "sandbox/" + resourceID, "failed to launch sandbox"} {
		if !strings.Contains(output, want) {
			t.Fatalf("jobs output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "UPDATED") || strings.Contains(output, "updatedAt") {
		t.Fatalf("jobs output = %q, did not expect updated column", output)
	}
}

func TestJobsParentQuietCommandPrintsFullIDsOnly(t *testing.T) {
	const jobID = "job_9qk5n25t2hh2rv00"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs" {
			t.Fatalf("path = %q, want project jobs path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":"` + jobID + `","type":"sandbox.reconcile","status":"pending","attempts":0,"resourceType":"sandbox","resourceId":"sandbox-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}]}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "jobs", "-q"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute jobs -q: %v", err)
	}
	if got := out.String(); got != jobID+"\n" {
		t.Fatalf("quiet output = %q, want full job ID only", got)
	}
}

// TestJobsTableSortsByRecencyDescending covers the default listing order shared
// by every CLI listing: most recently updated first, falling back to the
// creation time for records with no update time.
func TestJobsTableSortsByRecencyDescending(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	// Created last, but updated longest ago, so it sorts last.
	staleID := "job_9qk5n25t2hh2rv00"
	// Never updated, so its creation time orders it.
	createdOnlyID := "job_hqnk550g3821ck00"
	freshID := "job_rt3m5n8p1c42vk00"
	job := func(id, resource string, createdAt, updatedAt time.Time) apimodel.Job {
		return apimodel.Job{
			ID:           id,
			Type:         "worker.reconcile",
			Status:       apiclientgen.JobStatusCompleted,
			Attempts:     1,
			ResourceType: "worker",
			ResourceId:   resource,
			CreatedAt:    createdAt,
			UpdatedAt:    updatedAt,
		}
	}
	jobs := []apimodel.Job{
		job(staleID, "worker-stale", time.Date(2026, 6, 17, 3, 0, 0, 0, time.UTC), time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)),
		job(createdOnlyID, "worker-created-only", time.Date(2026, 6, 17, 6, 0, 0, 0, time.UTC), time.Time{}),
		job(freshID, "worker-fresh", time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 17, 8, 0, 0, 0, time.UTC)),
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	freshIndex := strings.Index(output, freshID)
	createdOnlyIndex := strings.Index(output, createdOnlyID)
	staleIndex := strings.Index(output, staleID)
	if freshIndex < 0 || createdOnlyIndex < 0 || staleIndex < 0 || freshIndex >= createdOnlyIndex || createdOnlyIndex >= staleIndex {
		t.Fatalf("jobs output = %q, want most recently updated first", output)
	}
	if jobs[0].ID != staleID {
		t.Fatalf("writeJobs mutated input order: %#v", jobs)
	}
}

func TestJobsTableShowsFutureSchedule(t *testing.T) {
	app := &App{output: "table"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	jobID := "job_9qk5n25t2hh2rv00"
	jobs := []apimodel.Job{
		{
			ID:           jobID,
			Type:         "workerprovider.reconcile",
			Status:       apiclientgen.JobStatusBackoff,
			Attempts:     1,
			ResourceType: "provider",
			ResourceId:   "provider-1",
			ScheduledAt:  time.Now().Add(5 * time.Minute),
			CreatedAt:    time.Now().Add(-1 * time.Minute),
		},
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	for _, want := range []string{"NEXT", jobID, "backoff", "5 minutes from now"} {
		if !strings.Contains(output, want) {
			t.Fatalf("jobs output = %q, want %q", output, want)
		}
	}
}

func TestJobsJSONPreservesResponseOrder(t *testing.T) {
	app := &App{output: "json"}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)

	newerID := "job_9qk5n25t2hh2rv00"
	olderID := "job_hqnk550g3821ck00"
	jobs := []apimodel.Job{
		{ID: newerID, CreatedAt: time.Date(2026, 6, 17, 1, 0, 0, 0, time.UTC)},
		{ID: olderID, CreatedAt: time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC)},
	}

	if err := app.writeJobs(cmd, jobs); err != nil {
		t.Fatalf("writeJobs: %v", err)
	}
	output := out.String()
	newerIndex := strings.Index(output, newerID)
	olderIndex := strings.Index(output, olderID)
	if newerIndex < 0 || olderIndex < 0 || newerIndex > olderIndex {
		t.Fatalf("jobs json output = %q, want response order preserved", output)
	}
}

func TestTruncateTableValueCompactsLongValues(t *testing.T) {
	got := truncateTableValue("first line\nsecond line with a lot of additional detail that should not wrap the table output in normal terminals", 80)
	if strings.Contains(got, "\n") {
		t.Fatalf("truncated value contains newline: %q", got)
	}
	if utf8.RuneCountInString(got) > 80 {
		t.Fatalf("truncated value length = %d, want <= 80: %q", utf8.RuneCountInString(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated value = %q, want ellipsis suffix", got)
	}
}

func TestJobsTableErrorWidthUsesTerminalWidth(t *testing.T) {
	rows := [][]string{{
		"n25t2hh2",
		"sandbox.reconcile",
		"failed",
		"1/1",
		"sandbox/50g3821ck",
		"1 minute ago",
	}}
	got := jobsTableErrorWidth(120, rows)
	if got <= 20 || got >= 80 {
		t.Fatalf("jobsTableErrorWidth() = %d, want terminal-derived width", got)
	}
	if minWidth := jobsTableErrorWidth(40, rows); minWidth != 20 {
		t.Fatalf("jobsTableErrorWidth(small terminal) = %d, want min width 20", minWidth)
	}
	if fallback := jobsTableErrorWidth(0, rows); fallback != 80 {
		t.Fatalf("jobsTableErrorWidth(no terminal) = %d, want fallback width 80", fallback)
	}
}

func TestFormatRelativeTime(t *testing.T) {
	now := time.Date(2026, 6, 17, 4, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		value time.Time
		want  string
	}{
		{name: "second", value: now.Add(-1 * time.Second), want: "1 second ago"},
		{name: "seconds", value: now.Add(-12 * time.Second), want: "12 seconds ago"},
		{name: "minute", value: now.Add(-1 * time.Minute), want: "1 minute ago"},
		{name: "minutes", value: now.Add(-12 * time.Minute), want: "12 minutes ago"},
		{name: "hour", value: now.Add(-1 * time.Hour), want: "1 hour ago"},
		{name: "hours", value: now.Add(-12 * time.Hour), want: "12 hours ago"},
		{name: "day", value: now.Add(-24 * time.Hour), want: "1 day ago"},
		{name: "future", value: now.Add(2 * time.Minute), want: "2 minutes from now"},
		{name: "zero", value: time.Time{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatRelativeTime(now, tt.value); got != tt.want {
				t.Fatalf("formatRelativeTime() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSandboxGetResolvesShortID(t *testing.T) {
	const fullID = "sbx_9qk5n25t2hh2rv00"
	sandboxJSON := testSandboxJSON(fullID, "alpha", "2026-06-17T00:00:00Z", "2026-06-17T00:00:01Z")
	var sawList bool
	var sawGet bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/projects/project-1/sandboxes":
			sawList = true
			_, _ = w.Write([]byte(`{"sandboxes":[` + sandboxJSON + `]}`))
		case "/projects/project-1/sandboxes/" + fullID:
			sawGet = true
			_, _ = w.Write([]byte(sandboxJSON))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "box", "get", "sbx_9qk5"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute sandbox get: %v", err)
	}
	if !sawList || !sawGet {
		t.Fatalf("sawList=%t sawGet=%t, want both", sawList, sawGet)
	}
	if output := out.String(); !strings.Contains(output, fullID) {
		t.Fatalf("sandbox output = %q, want full ID", output)
	}
}

func TestResolveShortIDMatchesPrefix(t *testing.T) {
	const fullID = "sbx_9qk5n25t2hh2rv00"
	for _, value := range []string{"sbx_9qk5", "sbx_9qk5n25t2hh2rv00", "sbx_9"} {
		t.Run(value, func(t *testing.T) {
			got, err := resolveShortID(value, "sandbox ID", []string{fullID})
			if err != nil {
				t.Fatalf("resolve short id: %v", err)
			}
			if got != fullID {
				t.Fatalf("resolved = %q, want %q", got, fullID)
			}
		})
	}
}

func TestSandboxDeleteContinuesAfterFailure(t *testing.T) {
	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodDelete {
			t.Fatalf("method = %q, want DELETE", got)
		}
		const prefix = "/projects/project-1/sandboxes/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Fatalf("path = %q, want sandbox delete path", r.URL.Path)
		}
		id := strings.TrimPrefix(r.URL.Path, prefix)
		deleted = append(deleted, id)
		if id == "sandbox-fail" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"title":"delete failed","detail":"sandbox is busy"}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "box", "delete", "sandbox-ok-1", "sandbox-fail", "sandbox-ok-2"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("execute sandbox delete error = nil, want aggregate delete failure")
	}
	if got, want := err.Error(), "failed to archive 1 discobox"; got != want {
		t.Fatalf("execute sandbox delete error = %q, want %q", got, want)
	}
	if got, want := strings.Join(deleted, ","), "sandbox-ok-1,sandbox-fail,sandbox-ok-2"; got != want {
		t.Fatalf("deleted order = %q, want %q", got, want)
	}
	if got, want := out.String(), "sandbox-ok-1 archived\nsandbox-ok-2 archived\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	for _, want := range []string{`failed to archive discobox "sandbox-fail"`, "sandbox is busy"} {
		if !strings.Contains(errOut.String(), want) {
			t.Fatalf("stderr = %q, want %q", errOut.String(), want)
		}
	}
}

func TestJobGetCommandShowsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/projects/project-1/jobs/job-1" {
			t.Fatalf("path = %q, want project job path", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","type":"worker.reconcile","status":"failed","attempts":1,"error":"container exited","message":"worker container exited","metadata":{"containerId":"abc123"},"resourceType":"worker","resourceId":"worker-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "job", "get", "job-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute job get: %v", err)
	}
	output := out.String()
	for _, want := range []string{"FIELD", "job-1", "failed", "worker/worker-1", "MESSAGE", "worker container exited", "METADATA", `"containerId":"abc123"`, "ERROR", "container exited"} {
		if !strings.Contains(output, want) {
			t.Fatalf("job output = %q, want %q", output, want)
		}
	}
}

func TestJobRunNowCommandForcesJob(t *testing.T) {
	var sawForce bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Method; got != http.MethodPost {
			t.Fatalf("method = %q, want POST", got)
		}
		if got := r.URL.Path; got != "/projects/project-1/jobs/job-1/force" {
			t.Fatalf("path = %q, want project job force path", got)
		}
		sawForce = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"job-1","type":"worker.reconcile","status":"pending","attempts":1,"resourceType":"worker","resourceId":"worker-1","scheduledAt":"2026-06-17T00:00:00Z","createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "job", "run-now", "job-1"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute job run-now: %v", err)
	}
	if !sawForce {
		t.Fatal("job force route was not called")
	}
	output := out.String()
	for _, want := range []string{"FIELD", "job-1", "pending", "worker/worker-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("job output = %q, want %q", output, want)
		}
	}
}

func TestProjectIDRejectsEmptyExplicitProject(t *testing.T) {
	app := &App{projectID: " "}
	if _, err := app.projectIDValue(); !errors.Is(err, errMissingProject) {
		t.Fatalf("projectIDValue error = %v, want errMissingProject", err)
	}
}

func TestHTTPClientAddsAuthorizationHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	app := &App{serverURL: server.URL, token: "token-1"}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, client, err := app.httpClient()
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
}

func TestDebugTransportPrintsRequestAndRedactsAuthorization(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer token-1" {
				t.Fatalf("authorization header = %q, want bearer token", got)
			}
			return &http.Response{
				Status:     "204 No Content",
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://user:pass@example.test/path?x=1", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer token-1")
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> POST http://user:xxxxx@example.test/path?x=1",
		"> Authorization: [REDACTED]",
		"> Content-Type: application/json",
		"< 204 No Content",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") || strings.Contains(output, "pass@example") {
		t.Fatalf("debug output leaked secret: %q", output)
	}
}

func TestHTTPClientDebugLogsAddedHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-1" {
			t.Fatalf("authorization header = %q, want bearer token", got)
		}
	}))
	t.Cleanup(server.Close)

	var log bytes.Buffer
	app := &App{serverURL: server.URL, token: "token-1", debug: true, errOut: &log}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, client, err := app.httpClient()
	if err != nil {
		t.Fatalf("http client: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	output := log.String()
	for _, want := range []string{
		"> GET " + server.URL,
		"> Authorization: [REDACTED]",
		"< 200 OK",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "token-1") {
		t.Fatalf("debug output leaked token: %q", output)
	}
}

func TestDebugTransportPrintsRequestAndResponseBodies(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Fatalf("read request body: %v", err)
			}
			if got, want := string(body), `{"name":"sandbox-1"}`; got != want {
				t.Fatalf("request body = %q, want %q", got, want)
			}
			return &http.Response{
				Status:     "201 Created",
				StatusCode: http.StatusCreated,
				Body:       io.NopCloser(strings.NewReader(`{"id":"sandbox-1"}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://example.test/sandboxes", strings.NewReader(`{"name":"sandbox-1"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}
	if got, want := string(respBody), `{"id":"sandbox-1"}`; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}

	output := log.String()
	for _, want := range []string{
		"> body:\n{\"name\":\"sandbox-1\"}\n",
		"< body:\n{\"id\":\"sandbox-1\"}\n",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("debug output = %q, want %q", output, want)
		}
	}
}

func TestDebugTransportDoesNotAddExtraResponseBodyNewline(t *testing.T) {
	var log bytes.Buffer
	transport := debugTransport{
		out: &log,
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				Status:     "200 OK",
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("{\"ok\":true}\n")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}),
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://example.test/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close body: %v", err)
	}

	if got, want := log.String(), "> GET http://example.test/healthz\n< 200 OK\n< body:\n{\"ok\":true}\n"; got != want {
		t.Fatalf("debug output = %q, want %q", got, want)
	}
}

func TestRootCommandIncludesServerSubcommand(t *testing.T) {
	cmd := NewRootCommand()
	found, _, err := cmd.Find([]string{"admin", "server"})
	if err != nil {
		t.Fatalf("find server command: %v", err)
	}
	if found == nil || found.Name() != "server" {
		t.Fatalf("server command = %v, want server", found)
	}
	found, _, err = cmd.Find([]string{"admin", "server", "shutdown"})
	if err != nil {
		t.Fatalf("find server shutdown command: %v", err)
	}
	if found == nil || found.Name() != "shutdown" {
		t.Fatalf("server shutdown command = %v, want shutdown", found)
	}
}

func TestServerShutdownWaitCommandWaitsForServerToStop(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/shutdown":
			w.WriteHeader(http.StatusAccepted)
			go func() {
				time.Sleep(20 * time.Millisecond)
				server.Close()
			}()
		case r.Method == http.MethodGet && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "admin", "server", "shutdown", "--wait"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute server shutdown --wait: %v", err)
	}
	if got := out.String(); got != "shutdown complete\n" {
		t.Fatalf("output = %q, want shutdown complete", got)
	}
}

func TestServerShutdownFallsBackToDefaultHTTPWhenSocketMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/shutdown" {
			t.Fatalf("request = %s %s, want POST /shutdown", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("PORT", serverURL.Port())

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"admin", "server", "shutdown"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute server shutdown fallback: %v", err)
	}
	if got := out.String(); got != "shutdown requested\n" {
		t.Fatalf("output = %q, want shutdown requested", got)
	}
}

func TestLocalServerEnvIncludesSupportedServerConfig(t *testing.T) {
	t.Setenv("DISCOBOX_DATA_DIR", "/tmp/discobox-data")
	t.Setenv("DISCOBOX_TOKEN", "client-token")

	env := localServerEnv("unix:///tmp/discobox/server.sock")
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"\nDISCOBOX_SERVER_LISTEN=unix:///tmp/discobox/server.sock\n",
		"\nDISCOBOX_SERVER=unix:///tmp/discobox/server.sock\n",
		"\nDISCOBOX_DATA_DIR=/tmp/discobox-data\n",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("localServerEnv() missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "\nDISCOBOX_TOKEN=client-token\n") {
		t.Fatalf("localServerEnv() included client token")
	}
	// A server the CLI launches for itself opens no TCP port on the user's
	// machine; HTTP is something an operator asks for explicitly.
	if strings.Contains(joined, "http://") {
		t.Fatalf("localServerEnv() configured an HTTP listener in %q", joined)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testSandboxJSON(id, name, createdAt, updatedAt string) string {
	return `{"id":"` + id + `","projectId":"project-1","createdByUserId":"user-1","displayName":"` + name + `","config":{"name":"` + name + `","image":""},"runtime":{"state":"ready","runtimeState":"running","desiredState":"present","displayState":"running","generation":1,"observedGeneration":1},"createdAt":"` + createdAt + `","updatedAt":"` + updatedAt + `"}`
}

// Bare `discobox` opens the launcher at a terminal and prints its help anywhere
// else: a full-screen window is not an answer to a program that expected
// output. Here the streams are buffers, which is the "anywhere else" case.
func TestBareCommandPrintsHelpWithoutATerminal(t *testing.T) {
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(&bytes.Buffer{})
	cmd.SetArgs(nil)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(out.String(), "Available Commands:") {
		t.Fatalf("want the help, got:\n%s", out.String())
	}
	// And nothing tried to reach a server on the way there.
	if strings.Contains(out.String(), "connect") {
		t.Fatalf("the help path should not have talked to anything:\n%s", out.String())
	}
}

// Any word after the bare command is a prompt, exactly as it would be after
// `run`: the shortcut is reaching the same run without its name, so a typo of
// a subcommand now creates a discobox prompted with the typo rather than
// reporting "unknown command". There is no rule that can tell the two apart —
// a prompt is words, and a subcommand name is indistinguishable from the first
// word of one — so this is the trade the shortcut makes.
func TestBareWordsAreARunPrompt(t *testing.T) {
	serveSSHSync := preparePromptCreateSSHSync(t)
	repo := newRunSourceTestRepo(t)
	var posted map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveSSHSync(w, r) {
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/projects/project-1/sandboxes" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&posted); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":"sbx_bogus","projectId":"project-1","createdByUserId":"user-1","displayName":"run-test","config":{"name":"run-test","image":""},"runtime":{"state":"pending","desiredState":"present","generation":1,"observedGeneration":0},"createdAt":"2026-06-17T00:00:00Z","updatedAt":"2026-06-17T00:00:01Z"}`))
	}))
	t.Cleanup(server.Close)

	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "-C", repo + "@HEAD", "-d", "bogus"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	config := posted["config"].(map[string]any)
	prompt, ok := config["prompt"].([]any)
	if !ok || len(prompt) != 1 || prompt[0] != "bogus" {
		t.Fatalf("prompt = %#v, want [bogus]", config["prompt"])
	}
}
