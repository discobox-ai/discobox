package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/id"
	"github.com/spf13/cobra"
)

type runOptions struct {
	source string
	ref    string
	prompt []string
	env    []string
	agent  string
	detach bool
}

func (a *App) newRunCommand() *cobra.Command {
	var opts runOptions
	cmd := &cobra.Command{
		Use:   "run [flags] SOURCE[@REF] [PROMPT...]",
		Short: "Run a prompt against a local directory or Git repository",
		Long: `Run a prompt against a local directory or Git repository.

The run command is intentionally shaped like docker run: command flags come first,
then the source directory or Git repository, followed by the prompt. Use -- when
the source or prompt needs to be separated from command flags explicitly.

By default run waits for the sandbox to start and attaches to its default agent
terminal, streaming it to your terminal (press Ctrl-P Ctrl-Q to detach). Pass -d
to create the sandbox and print it without attaching.`,
		Example: `  discobox run . fix the failing tests
  discobox run https://github.com/obot-platform/discobox.git@main summarize the CLI package
  discobox run -e GITHUB_TOKEN -e MODE=test . fix the failing tests
  discobox run -d . fix the failing tests
  discobox run -- . prompt starting with --flag-like text`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsedOpts, err := parseRunArgs(args)
			if err != nil {
				return err
			}
			parsedOpts.env = append(parsedOpts.env, opts.env...)
			parsedOpts.agent = opts.agent
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			body, err := createRunSandboxBody(cmd.Context(), parsedOpts)
			if err != nil {
				return err
			}
			sandboxRes, err := client.CreateSandbox(cmd.Context(), body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			if opts.detach {
				return a.writeSandbox(cmd, sandbox)
			}
			return a.attachRunSandbox(cmd, client, projectID, sandbox)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	cmd.Flags().StringVarP(&opts.agent, "agent", "a", "", "Agent config to run, by slug (e.g. codex), name, or ID; defaults to the project default")
	cmd.Flags().BoolVarP(&opts.detach, "detach", "d", false, "Create the sandbox and print it without attaching to its terminal")
	return cmd
}

// attachRunSandbox waits for the freshly created sandbox to start and for its
// default terminal to come up, then attaches the caller's stdio to it. The
// sandbox-agent always launches one primary terminal running the configured
// agent, so run attaches to it unless --detach was passed.
func (a *App) attachRunSandbox(cmd *cobra.Command, client *apiclientgen.Client, projectID string, sandbox *apimodel.Sandbox) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()
	fmt.Fprintf(stderr, "Created sandbox %s, waiting for it to start...\n", shortID(sandbox.ID))
	started, err := a.waitForSandbox(cmd, client, projectID, sandbox.ID, 2*time.Minute)
	if err != nil {
		return err
	}
	// Fail fast when the running sandbox has no agent to launch. The sandbox-agent
	// only starts a primary terminal when it can resolve an agent, so without one
	// waitForPrimaryTerminal below would block until its timeout for a terminal
	// that never appears.
	if err := ensureRunAgentWillLaunch(started); err != nil {
		return err
	}
	terminal, err := a.waitForPrimaryTerminal(ctx, client, projectID, sandbox.ID, 2*time.Minute)
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Attaching to terminal %s (Ctrl-P Ctrl-Q to detach)\n", shortID(terminal.ID))
	// Replay the primary terminal's saved history first: the sandbox-agent
	// launches and drives it before run connects, so replay shows the session
	// from the start rather than only output produced after the attach.
	return a.attachSandboxTerminal(ctx, projectID, sandbox.ID, terminal.ID, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// errNoRunAgent explains that a sandbox has no agent and how to configure one. It
// is returned when run can prove no primary terminal will ever launch.
var errNoRunAgent = errors.New("no agent is configured for this sandbox: enable one with `discobox agents enable <definition>` (see `discobox agents definitions`), set a project default with `discobox agents set-default <id>`, or pass --agent")

// ensureRunAgentWillLaunch reports whether a freshly started sandbox will launch
// a primary terminal. The sandbox-agent resolves the agent in this precedence:
// the sandbox's resolved agent config, a local repo agent config, then the
// project default. When the server pinned an agent config the terminal will
// launch. Otherwise the only remaining source is a local repo agent config,
// which run can check for local sources; remote sources are resolved
// sandbox-side, so those defer to the bounded wait rather than fail here.
func ensureRunAgentWillLaunch(sandbox *apimodel.Sandbox) error {
	if strings.TrimSpace(sandbox.Config.AgentConfigId.Or("")) != "" {
		return nil
	}
	source, ok := sandbox.Config.Source.Get()
	if !ok {
		return nil
	}
	localDir := strings.TrimSpace(source.LocalDirectory.Or(""))
	if localDir == "" || localRunAgentConfigPresent(localDir) {
		return nil
	}
	return errNoRunAgent
}

// localRunAgentConfigPresent reports whether a local source directory carries a
// .discobox agent config. It mirrors the sandbox-agent's local agent config
// lookup (sandbox-agent/terminal/service.go localAgentConfigPath) so run can
// predict whether that path will supply an agent.
func localRunAgentConfigPresent(repoRoot string) bool {
	for _, name := range []string{"agent.json", "agent-config.json", "sandbox.json"} {
		if info, err := os.Stat(filepath.Join(repoRoot, ".discobox", name)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

// waitForPrimaryTerminal polls the sandbox terminals until the primary
// (default) terminal launched by the sandbox-agent appears.
func (a *App) waitForPrimaryTerminal(ctx context.Context, _ *apiclientgen.Client, projectID, sandboxID string, timeout time.Duration) (apimodel.SandboxExec, error) {
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if terminals, err := a.listSandboxTerminals(ctx, projectID, sandboxID); err != nil {
			lastErr = err
		} else if terminal, ok := primaryTerminal(terminals); ok {
			return terminal, nil
		} else {
			lastErr = nil
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return apimodel.SandboxExec{}, fmt.Errorf("waiting for sandbox terminal: %w", lastErr)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return apimodel.SandboxExec{}, errors.New("timed out waiting for the sandbox's agent terminal; the sandbox may have no agent configured (see `discobox agents list`) or its agent failed to start")
			}
			return apimodel.SandboxExec{}, fmt.Errorf("waiting for sandbox terminal: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// primaryTerminal selects the sandbox's default terminal: the one flagged
// primary by the sandbox-agent, falling back to the oldest terminal.
func primaryTerminal(terminals []apimodel.SandboxExec) (apimodel.SandboxExec, bool) {
	var oldest *apimodel.SandboxExec
	for i := range terminals {
		if terminals[i].Primary.Or(false) {
			return terminals[i], true
		}
		if oldest == nil || terminals[i].CreatedAt.Before(oldest.CreatedAt) {
			oldest = &terminals[i]
		}
	}
	if oldest != nil {
		return *oldest, true
	}
	return apimodel.SandboxExec{}, false
}

func parseRunArgs(args []string) (runOptions, error) {
	if len(args) == 0 {
		return runOptions{}, errors.New("source directory or Git repository is required")
	}
	opts := runOptions{
		source: args[0],
		prompt: append([]string(nil), args[1:]...),
	}
	if opts.source == "" {
		return runOptions{}, errors.New("source directory or Git repository is required")
	}
	if source, ref, ok := splitRunSourceRef(opts.source); ok {
		opts.source = source
		opts.ref = ref
	}
	return opts, nil
}

func splitRunSourceRef(value string) (string, string, bool) {
	at := strings.LastIndex(value, "@")
	if at <= 0 || at == len(value)-1 {
		return value, "", false
	}
	if !strings.Contains(value[:at], "@") && strings.Contains(value[at+1:], ":") {
		return value, "", false
	}
	return value[:at], value[at+1:], true
}

func createRunSandboxBody(ctx context.Context, opts runOptions) (*apimodel.CreateSandboxBody, error) {
	runID, err := id.New()
	if err != nil {
		return nil, err
	}
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: "run-" + shortID(runID)}}
	if len(opts.prompt) > 0 {
		body.Config.SetPrompt(append([]string(nil), opts.prompt...))
	}
	if strings.TrimSpace(opts.agent) != "" {
		body.SetAgentName(optString(opts.agent))
	}
	env, err := keyValueMapFromShell(opts.env)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.Config.SetEnv(apiclientgen.NewOptSandboxCreateConfigEnv(apiclientgen.SandboxCreateConfigEnv(env)))
	}
	userIdentity, _, err := resolveRunUserIdentity()
	if err != nil {
		return nil, err
	}
	sourceArg := opts.source
	if opts.ref != "" {
		sourceArg += "@" + opts.ref
	}
	source, err := resolveRunSource(ctx, sourceArg)
	if err != nil {
		return nil, err
	}
	apiSource, err := source.apiGitSource()
	if err != nil {
		return nil, err
	}
	body.Config.SetSource(apiclientgen.NewOptGitSource(*apiSource))
	userIdentity.setCreateSandboxUser(body)
	return body, nil
}
