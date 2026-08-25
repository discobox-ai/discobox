package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/keys"
	"github.com/discobox-ai/discobox/controlplane"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/health"
	discoboxserver "github.com/discobox-ai/discobox/server"
	"github.com/discobox-ai/discobox/version"
)

const defaultProjectAlias = "default"

type App struct {
	serverURL string
	projectID string
	source    string
	token     string
	output    string
	quiet     bool
	debug     bool
	noStart   bool
	errOut    io.Writer

	// leaderKey is the prefix key this invocation reserves in a terminal it
	// shows: the launcher's window commands, and the detach chord of an attach.
	// validate resolves it from the environment, so it is empty until then —
	// read it through leader() rather than directly.
	leaderKey string

	// serverCmd is the command that runs the server, kept so the autolaunch
	// can ask the command tree where it lives instead of naming it. See
	// serverLaunchArgs.
	serverCmd *cobra.Command
}

func NewRootCommand() *cobra.Command {
	cmd, _ := newRootCommand()
	return cmd
}

// newRootCommand is NewRootCommand with the App it wired, so a test can ask the
// app about the tree it is part of.
func newRootCommand() (*cobra.Command, *App) {
	cobra.EnableCommandSorting = false

	app := &App{}
	cmd := &cobra.Command{
		Use:           "discobox",
		Short:         "Discobox command line client",
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			app.errOut = cmd.ErrOrStderr()
			return app.validate()
		},
		// Bare `discobox` at a terminal opens the launcher: it is the one thing
		// you can ask for without knowing a subcommand, and typing the name of
		// a program is how you ask for it. Anywhere else — a pipe, a script,
		// CI — it prints its help, because a full-screen window is not an
		// answer to a program that expected output.
		//
		// Args is deliberately left alone: cobra's default already turns an
		// unrecognized first argument into "unknown command" rather than
		// handing it here, and that error is worth more than a launcher that
		// opens when you misspell one.
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !isTerminalStream(cmd.OutOrStdout()) || !isTerminalStream(cmd.InOrStdin()) {
				return cmd.Help()
			}
			// No leader override here: validate already took the environment's,
			// and --leader is the tui command's own. A flag would have to be a
			// persistent one to be reachable from a bare `discobox`, and every
			// subcommand would then carry a flag that means nothing to it.
			return app.runTUI(cmd, "")
		},
	}
	cmd.PersistentFlags().StringVar(&app.serverURL, "server", envOrDefault("DISCOBOX_SERVER", endpoint.DefaultEndpoint()), "Discobox API server endpoint")
	cmd.PersistentFlags().StringVarP(&app.projectID, "project", "p", envOrDefault("DISCOBOX_PROJECT", defaultProjectAlias), "Project ID for this invocation; use default for the user's default project")
	cmd.PersistentFlags().StringVarP(&app.source, "chdir", "C", ".", "Source directory or Git repository to act on, optionally with @REF; its Git repository root identifies the discoboxes ls lists and run creates")
	// Beta: the flag works but is undocumented until the source-selection UX is
	// settled, so it stays out of help text and examples.
	_ = cmd.PersistentFlags().MarkHidden("chdir")
	cmd.PersistentFlags().StringVar(&app.token, "token", os.Getenv("DISCOBOX_TOKEN"), "Bearer token for API requests")
	cmd.PersistentFlags().StringVarP(&app.output, "output", "o", "table", "Output format: table or json")
	cmd.PersistentFlags().BoolVar(&app.debug, "debug", false, "Print HTTP requests made by the API client, and the git commands run on this machine")
	cmd.PersistentFlags().BoolVar(&app.noStart, "no-start", false, "Do not start a local server when the endpoint is unavailable")
	_ = cmd.RegisterFlagCompletionFunc("project", app.completeProjects)

	cmd.AddCommand(app.newRunCommand())
	cmd.AddCommand(app.newListCommand())
	cmd.AddCommand(app.newShellCommand())
	cmd.AddCommand(app.newAttachCommand())
	cmd.AddCommand(app.newProxyCommand())
	cmd.AddCommand(app.newApplyCommand())
	cmd.AddCommand(app.newPushCommand())
	cmd.AddCommand(app.newToolsCommand())
	cmd.AddCommand(app.newConfigureCommand())
	cmd.AddCommand(app.newSecretCommand())
	cmd.AddCommand(app.newTUICommand())
	cmd.AddCommand(app.newCompletionCommand())
	cmd.AddCommand(app.newAdminCommand())
	// Cobra's usage template always lists a subcommand literally named "help",
	// even when hidden. Give the help command another name so it stays out of the
	// command list; the --help flag still works on every command.
	cmd.SetHelpCommand(&cobra.Command{Use: "no-help", Hidden: true})
	return cmd, app
}

func (a *App) addQuietFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&a.quiet, "quiet", "q", false, "Only display resource IDs")
}

func (a *App) validate() error {
	switch a.output {
	case "table", "json":
	default:
		return fmt.Errorf("unsupported output format %q; expected table or json", a.output)
	}
	leaderKey, err := keys.NormalizeLeader(os.Getenv(keys.LeaderEnv))
	if err != nil {
		// Resolved once, for every command, rather than at the attach that
		// happens to need it: a leader the environment spells wrong is worth
		// hearing about before a terminal has been handed over.
		return fmt.Errorf("%s: %w", keys.LeaderEnv, err)
	}
	a.leaderKey = leaderKey
	return nil
}

// leader is the prefix key this invocation reserves, as a Bubble Tea key name.
//
// One key covers both terminals discobox shows you — the launcher's panes and a
// plain attach — so there is one thing to learn and one thing to change when it
// collides with what you run inside a sandbox.
func (a *App) leader() string {
	if a.leaderKey == "" {
		return keys.DefaultLeader
	}
	return a.leaderKey
}

func (a *App) projectIDValue() (string, error) {
	projectID := strings.TrimSpace(a.projectID)
	if projectID == "" {
		return "", errMissingProject
	}
	return projectID, nil
}

func (a *App) apiClient() (*apiclientgen.Client, error) {
	baseURL, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	return apiclientgen.NewClient(baseURL, apiclientgen.WithClient(client))
}

func (a *App) httpClient() (string, *http.Client, error) {
	return a.httpClientWithAutoStart(shouldAutoLaunchServer(a.noStart))
}

func shouldAutoLaunchServer(noStart bool) bool {
	return serverAutoLaunch == "true" && !noStart
}

func (a *App) httpClientWithAutoStart(autoStart bool) (string, *http.Client, error) {
	transport := http.DefaultTransport
	parsed := a.serverEndpoint()
	if autoStart && parsed.AutoLaunchable() {
		if err := a.ensureLocalServer(context.Background()); err != nil {
			return "", nil, err
		}
	}
	if err := configureIrohForEndpoint(parsed); err != nil {
		return "", nil, err
	}
	baseURL, client, err := endpoint.HTTPClient(a.serverURL, transport)
	if err != nil {
		return "", nil, err
	}
	transport = client.Transport
	if a.debug {
		transport = debugTransport{
			out:  a.errOut,
			base: transport,
		}
	}
	transport = textPlainErrorTransport{
		base: transport,
	}
	if strings.TrimSpace(a.token) != "" {
		transport = requestHeaderTransport{
			token: strings.TrimSpace(a.token),
			base:  transport,
		}
	}
	if transport == http.DefaultTransport {
		return baseURL, http.DefaultClient, nil
	}
	return baseURL, &http.Client{
		Transport: transport,
	}, nil
}

// gitServerURL is the base URL git commands address the server through, along
// with the func that releases it.
//
// git only speaks URLs, so a unix socket or named pipe endpoint has to be
// bridged: the returned URL is a loopback proxy onto the same local server the
// API client uses. An http(s) endpoint is already addressable and is returned
// as-is, with nothing to release.
func (a *App) gitServerURL(ctx context.Context) (string, func(), error) {
	if a.serverEndpoint().DirectlyDialable() {
		return a.serverURL, func() {}, nil
	}
	proxy, err := endpoint.StartLoopbackProxy(ctx, a.serverURL)
	if err != nil {
		return "", nil, err
	}
	return proxy.BaseURL(), func() { _ = proxy.Close() }, nil
}

// serverEndpoint parses --server so callers can ask what the endpoint supports
// rather than pattern-matching its scheme. A malformed endpoint answers "no" to
// every capability; the error itself surfaces from the call that goes on to use
// the endpoint, where it can say what failed.
func (a *App) serverEndpoint() endpoint.Endpoint {
	parsed, err := endpoint.Parse(a.serverURL)
	if err != nil {
		return endpoint.Endpoint{}
	}
	return parsed
}

func (a *App) ensureLocalServer(ctx context.Context) error {
	command, err := os.Executable()
	if err != nil {
		return err
	}
	args, err := a.serverLaunchArgs()
	if err != nil {
		return err
	}
	started, err := endpoint.EnsureRunning(ctx, endpoint.LaunchOptions{
		Endpoint:   a.serverURL,
		Command:    command,
		Args:       args,
		Env:        localServerEnv(a.serverURL),
		OnProgress: a.reportServerStartup,
	})
	if err != nil {
		return err
	}
	if started {
		// A server started this way outlives the command that wanted it, which
		// is the point and also something the user did not ask for. Say that it
		// happened, where its output went — it has no terminal, so the file is
		// the only place it exists — and how to undo it. A caller that found one
		// already up says nothing.
		a.notify("started the discobox server in the background (logs: discobox admin server logs; stop it with: discobox admin server shutdown)")
	}
	return nil
}

// reportServerStartup says what the server this CLI just launched is doing.
//
// Starting one can take a while on a first run — a database to migrate, a
// registry to reach for the built-in harness images — and the whole of that
// used to be silent, so the only two outcomes a user saw were a prompt that
// came back late and a timeout that explained nothing.
func (a *App) reportServerStartup(status health.Status) {
	phase := status.Phase
	if phase == "" {
		phase = "starting"
	}
	a.notify("starting discobox server: " + phase)
}

// notify writes one line about something done on the user's behalf.
func (a *App) notify(text string) {
	if a.quiet || a.errOut == nil {
		return
	}
	fmt.Fprintln(a.errOut, text)
}

// localServerEnv configures the server this CLI launches for itself. It listens
// on the endpoint the CLI is about to dial and nothing else: an autolaunched
// server is this user's, and opening a TCP port on their machine is not
// something running `discobox` should imply. An operator who needs HTTP names it
// in DISCOBOX_SERVER_LISTEN and runs the server themselves.
func localServerEnv(endpoint string) []string {
	env := []string{
		"DISCOBOX_SERVER_LISTEN=" + endpoint,
		"DISCOBOX_SERVER=" + endpoint,
		"DISCOBOX_SERVER_IDLE_TIMEOUT=5m",
	}
	for _, key := range []string{
		"DATABASE_DSN",
		"DATABASE_READ_DSN",
		"DISCOBOX_CACHE_DIR",
		"DISCOBOX_CONFIG_DIR",
		"DISCOBOX_DATA_DIR",
		"DISCOBOX_DEFAULT_DISCOBOX_IMAGE",
		"DISCOBOX_ENCRYPTION_KEY",
		"DISCOBOX_STATE_DIR",
		"OTEL_METRICS_EXPORTER",
		"PATH",
		"PORT",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (a *App) newServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Discobox API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return discoboxserver.Run(cmd.Context())
		},
	}
	cmd.AddCommand(a.newServerShutdownCommand())
	cmd.AddCommand(a.newServerLogsCommand())
	a.serverCmd = cmd
	return cmd
}

// serverLaunchArgs is the argv this binary re-invokes itself with to run its
// own server, read off the command tree rather than written out.
//
// It used to be the literal []string{"server"}. The command moved under
// `admin`, the literal did not, and the autolaunched process exited instantly
// with `unknown command "server"` — invisibly, because its output goes nowhere
// — leaving every CLI invocation to wait out the start timeout and report that
// the server never became ready. A path spelled by hand is a reference the
// compiler cannot check; this one moves when the command does.
func (a *App) serverLaunchArgs() ([]string, error) {
	if a.serverCmd == nil {
		return nil, fmt.Errorf("server command is not wired into the command tree")
	}
	var path []string
	for cmd := a.serverCmd; cmd != nil && cmd.HasParent(); cmd = cmd.Parent() {
		path = append([]string{cmd.Name()}, path...)
	}
	if len(path) == 0 {
		return nil, fmt.Errorf("server command has no path in the command tree")
	}
	return path, nil
}

func (a *App) newServerShutdownCommand() *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:   "shutdown",
		Short: "Ask the Discobox API server to stop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			baseURL, httpClient, err := a.httpClientWithAutoStart(false)
			if err != nil {
				return err
			}
			resp, err := requestServerShutdown(cmd.Context(), baseURL, httpClient)
			if err != nil && a.serverEndpoint().AutoLaunchable() {
				baseURL, httpClient = defaultHTTPShutdownClient()
				resp, err = requestServerShutdown(cmd.Context(), baseURL, httpClient)
			}
			if err != nil {
				return fmt.Errorf("shutdown server: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("shutdown server: %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			if wait {
				if err := waitForServerShutdown(cmd.Context(), baseURL, httpClient, 10*time.Second); err != nil {
					return err
				}
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"shutdown": true, "wait": wait})
			}
			message := "shutdown requested"
			if wait {
				message = "shutdown complete"
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
			return err
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait until the server stops accepting requests")
	return cmd
}

func requestServerShutdown(ctx context.Context, baseURL string, httpClient *http.Client) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/shutdown", nil)
	if err != nil {
		return nil, err
	}
	return httpClient.Do(req)
}

func defaultHTTPShutdownClient() (string, *http.Client) {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = fmt.Sprint(controlplane.DefaultPort)
	}
	return "http://127.0.0.1:" + port, http.DefaultClient
}

func waitForServerShutdown(ctx context.Context, baseURL string, httpClient *http.Client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, baseURL+"/healthz", nil)
		if err != nil {
			cancel()
			return err
		}
		resp := doShutdownProbe(httpClient, req)
		cancel()
		if resp == nil {
			return nil
		}
		_ = resp.Body.Close()
		if time.Now().After(deadline) {
			return fmt.Errorf("shutdown server: still accepting requests after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func doShutdownProbe(httpClient *http.Client, req *http.Request) *http.Response {
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	return resp
}

type requestHeaderTransport struct {
	token string
	base  http.RoundTripper
}

func (t requestHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.token)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

type textPlainErrorTransport struct {
	base http.RoundTripper
}

func (t textPlainErrorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil || resp.StatusCode < 400 {
		return resp, err
	}
	ct, _, parseErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if parseErr != nil || ct != "text/plain" || resp.Body == nil {
		return resp, err
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return resp, readErr
	}
	if closeErr != nil {
		return resp, closeErr
	}
	return nil, fmt.Errorf("request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
}

type debugTransport struct {
	out  io.Writer
	base http.RoundTripper
}

func (t debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := t.out
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "> %s %s\n", req.Method, req.URL.Redacted())
	for name, values := range req.Header {
		for _, value := range values {
			if strings.EqualFold(name, "Authorization") {
				value = "[REDACTED]"
			}
			fmt.Fprintf(out, "> %s: %s\n", name, value)
		}
	}
	req, err := logRequestBody(out, req)
	if err != nil {
		fmt.Fprintf(out, "> body error: %v\n", err)
		return nil, err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(out, "< error: %v\n", err)
		return nil, err
	}
	fmt.Fprintf(out, "< %s\n", resp.Status)
	// A 101 hands the connection itself back as resp.Body, and the websocket
	// client takes it over by asserting io.ReadWriteCloser. Wrapping it fails
	// that assertion — every attach under --debug dies with "101 Switching
	// Protocols" — and would dump binary frames into the log besides.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		return resp, nil
	}
	if resp.Body != nil && resp.Body != http.NoBody {
		fmt.Fprintln(out, "< body:")
		resp.Body = &debugBody{out: out, base: resp.Body}
	}
	return resp, nil
}

func logRequestBody(out io.Writer, req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return req, err
	}
	if err := req.Body.Close(); err != nil {
		return req, err
	}
	cloned := req.Clone(req.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	cloned.ContentLength = int64(len(body))
	if len(body) > 0 {
		fmt.Fprintln(out, "> body:")
		_, _ = out.Write(body)
		if body[len(body)-1] != '\n' {
			fmt.Fprintln(out)
		}
	}
	return cloned, nil
}

type debugBody struct {
	out       io.Writer
	base      io.ReadCloser
	wroteBody bool
	lastByte  byte
}

func (b *debugBody) Read(p []byte) (int, error) {
	n, err := b.base.Read(p)
	if n > 0 {
		_, _ = b.out.Write(p[:n])
		b.wroteBody = true
		b.lastByte = p[n-1]
	}
	return n, err
}

func (b *debugBody) Close() error {
	if b.wroteBody && b.lastByte != '\n' {
		fmt.Fprintln(b.out)
	}
	return b.base.Close()
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
