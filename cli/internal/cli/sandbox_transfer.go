package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
)

// Moving a discobox between servers is three commands over two server-side
// routes (ADR 0123). The archive itself is opaque here: the server composes it
// and the server reads it, so this only ever moves bytes — which is what lets a
// CLI keep working against a server that changed the format.

const (
	// exportFileExtension is what an export is called on disk.
	exportFileExtension = ".dbox"
	// exportMediaType is what the routes on both ends speak.
	exportMediaType = "application/x-tar"
	// stopWaitTimeout bounds waiting for a discobox to come to a stop before it
	// is read. Generous: stopping runs the harness's own shutdown and flushes
	// its terminals.
	stopWaitTimeout = 2 * time.Minute
)

type sandboxImportResponse struct {
	Sandbox  *apimodel.Sandbox `json:"sandbox"`
	Warnings []string          `json:"warnings,omitempty"`
}

func (a *App) newSandboxExportCommand() *cobra.Command {
	var out string
	var stop bool
	cmd := &cobra.Command{
		Use:   "export DISCOBOX_ID",
		Short: "Write a discobox to a portable archive",
		Long: `Write a discobox to a .dbox archive that can be imported on another server.

The archive holds the discobox's spec and its durable data: the home directory,
the workspace with its full git history, and the origin repositories of any
push-delivered source. That is the same set a rebuild, a repair, or an upgrade
already preserves — so anything installed into the container outside those
directories is not in it, and is not in an upgraded discobox either. Put what
you need in the harness image.

No secret values are ever written to the archive. Bindings travel by name, and
the destination binds each to its own secret of that name.

A running discobox is refused: a copy taken while it is writing can catch a git
index or a database part way through, and you would not find out until you
imported it. Pass --stop to stop it first, which leaves it stopped.`,
		Example: `  discobox admin box export my-box
  discobox admin box export my-box --stop -o ~/backups/my-box.dbox
  discobox admin box export my-box -o - | gzip > my-box.dbox.gz`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sandbox, err := a.prepareSandboxForExport(cmd.Context(), client, projectID, sandboxID, stop, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			destination := strings.TrimSpace(out)
			if destination == "" {
				destination = exportDefaultFileName(sandbox)
			}
			return a.exportSandboxTo(cmd.Context(), projectID, sandboxID, destination, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "File to write; \"-\" writes to stdout (default \"<name>.dbox\")")
	cmd.Flags().BoolVar(&stop, "stop", false, "Stop the discobox first, and leave it stopped")
	return cmd
}

func (a *App) newSandboxImportCommand() *cobra.Command {
	var opts sandboxImportOptions
	cmd := &cobra.Command{
		Use:   "import FILE",
		Short: "Create a discobox from a portable archive",
		Long: `Create a discobox from a .dbox archive written by "discobox admin box export".

The discobox is recreated with the spec the archive carries and its data
restored onto this server's pool, then built the way an unarchived discobox is.
It gets a new ID here; the name comes from the archive unless --name says
otherwise, and a name this project already uses is refused.

The harness is resolved by name. This project must already have one configured
under the same name, or --harness must name the one to use instead: the harness
is what the discobox runs, so a substitute is not chosen for you.

Secret bindings are matched to this project's secrets by name. Any that cannot
be matched are reported, and the discobox is created without them.`,
		Example: `  discobox admin box import my-box.dbox
  discobox admin box import my-box.dbox --name my-box-2 --pool big-pool
  gunzip -c my-box.dbox.gz | discobox admin box import -`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			if opts.poolID != "" {
				client, err := a.apiClient()
				if err != nil {
					return err
				}
				opts.poolID, err = a.resolvePoolID(cmd.Context(), client, projectID, opts.poolID)
				if err != nil {
					return err
				}
			}
			archive, closeArchive, err := openImportArchive(args[0], cmd.InOrStdin())
			if err != nil {
				return err
			}
			defer closeArchive()
			result, err := a.importSandbox(cmd.Context(), projectID, archive, opts)
			if err != nil {
				return err
			}
			reportImportWarnings(cmd.ErrOrStderr(), result)
			return a.writeSandbox(cmd, result.Sandbox)
		},
	}
	addImportFlags(cmd, &opts)
	_ = cmd.RegisterFlagCompletionFunc("pool", a.completePools)
	return cmd
}

func (a *App) newSandboxTransferCommand() *cobra.Command {
	var opts sandboxImportOptions
	var to string
	var keep bool
	cmd := &cobra.Command{
		Use:   "transfer DISCOBOX_ID --to SERVER",
		Short: "Move a discobox to another server",
		Long: `Move a discobox to another registered server.

The discobox is stopped, its spec and durable data are streamed straight from
one server to the other through this command, and the source is archived once
the destination confirms it. The bytes are never written to disk here, so a
transfer needs no room for a copy.

Archiving the source keeps its data and drops its container, so a transfer that
turns out to be wrong is undone with "discobox admin box unarchive". The archive
is collected by the project's ordinary retention once that window passes. Pass
--keep to leave the source where it is, stopped, which makes this a copy.

What travels, what does not, and how the harness and secrets are resolved on the
far side are the same as for "export" and "import"; see those.

"discobox admin remote" lists the servers --to can name.`,
		Example: `  discobox admin box transfer my-box --to lab
  discobox admin box transfer my-box --to lab --keep
  discobox admin box transfer my-box --to lab --name my-box-2 --pool big-pool`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := a.transferTarget(to)
			if err != nil {
				return err
			}
			return a.transferSandbox(cmd, args[0], target, opts, keep)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "Registered server, or an address, to move the discobox to")
	cmd.Flags().BoolVar(&keep, "keep", false, "Leave the source discobox in place, stopped, instead of archiving it")
	addImportFlags(cmd, &opts)
	_ = cmd.MarkFlagRequired("to")
	_ = cmd.RegisterFlagCompletionFunc("to", completeServerNames(-1))
	return cmd
}

type sandboxImportOptions struct {
	name        string
	poolID      string
	harnessSlug string
}

func addImportFlags(cmd *cobra.Command, opts *sandboxImportOptions) {
	cmd.Flags().StringVar(&opts.name, "name", "", "Name for the new discobox (default: the name it had)")
	cmd.Flags().StringVar(&opts.poolID, "pool", "", "Pool to place it in (default: the destination project's default pool)")
	cmd.Flags().StringVar(&opts.harnessSlug, "harness", "", "Harness to run it with (default: the one it had, resolved by name)")
}

// prepareSandboxForExport refuses a running discobox, or stops it when asked.
//
// Stopping leaves it stopped rather than restoring it afterwards: an export is
// usually the first half of a move, and starting the source back up just to
// archive it a moment later is work nobody wanted. It is also the honest
// outcome — restarting would make the exported copy and the running one diverge
// from the moment the archive was taken.
func (a *App) prepareSandboxForExport(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, stop bool, stderr io.Writer) (*apimodel.Sandbox, error) {
	sandbox, err := getSandbox(ctx, client, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	if !sandboxIsRunning(sandbox) {
		return sandbox, nil
	}
	if !stop {
		return nil, fmt.Errorf("%s is running; stop it first, or pass --stop", sandboxLabel(sandbox))
	}
	fmt.Fprintf(stderr, "Stopping %s…\n", sandboxLabel(sandbox))
	if _, err := client.StopSandbox(ctx, &apimodel.StopSandboxBody{}, apiclientgen.StopSandboxParams{ProjectId: projectID, SandboxId: sandboxID}); err != nil {
		return nil, err
	}
	return waitForSandboxStopped(ctx, client, projectID, sandboxID)
}

// waitForSandboxStopped polls until the pool agent reports the container down.
//
// Stop is an instruction, not stored intent (ADR 0017 §9): the response says it
// was accepted and nothing more, so the only way to know the container has
// actually gone down is to read the state the agent reports afterwards. Reading
// the tree before then is exactly the inconsistency the refusal above exists to
// prevent.
func waitForSandboxStopped(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (*apimodel.Sandbox, error) {
	// The deadline is the wait's, not the requests'. Sharing one meant that
	// once it passed, `ctx.Done()` and the ticker were both ready and select
	// chose between them at random: half the time the next read ran on a dead
	// context and the user got a transport-shaped "context deadline exceeded"
	// instead of the sentence this loop exists to produce -- which in a
	// transfer is what tells them the discobox is stopped but not moved.
	deadline := time.NewTimer(stopWaitTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var last *apimodel.Sandbox
	for {
		sandbox, err := getSandbox(ctx, client, projectID, sandboxID)
		if err != nil {
			return nil, err
		}
		if !sandboxIsRunning(sandbox) {
			return sandbox, nil
		}
		last = sandbox
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("%s did not stop within %s", sandboxLabel(last), stopWaitTimeout)
		case <-ticker.C:
		}
	}
}

// sandboxIsRunning reads the observed power state. An absent one means no agent
// has reported on this discobox, which is not `stopped` — but it is also not a
// container writing to the tree, which is the only thing that matters here.
func sandboxIsRunning(sandbox *apimodel.Sandbox) bool {
	switch sandbox.Runtime.RuntimeState.Or("") {
	case "running", "starting", "stopping":
		return true
	}
	return false
}

func sandboxLabel(sandbox *apimodel.Sandbox) string {
	if name := strings.TrimSpace(sandbox.Config.Name); name != "" {
		return name
	}
	return sandbox.ID
}

func getSandbox(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (*apimodel.Sandbox, error) {
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	return expectResponse[apimodel.Sandbox](res)
}

// exportDefaultFileName is what an export is called when nobody said.
func exportDefaultFileName(sandbox *apimodel.Sandbox) string {
	name := strings.TrimSpace(sandbox.Config.Name)
	if name == "" {
		name = sandbox.ID
	}
	// One path element: a discobox may be named anything, and this becomes a
	// file in the working directory.
	name = strings.ReplaceAll(strings.ReplaceAll(name, string(filepath.Separator), "-"), "/", "-")
	if name == "" || name == "." || name == ".." {
		name = sandbox.ID
	}
	return name + exportFileExtension
}

// exportSandboxTo streams the export to a file, or to stdout for "-".
//
// The body is written as it arrives rather than buffered: a workspace is
// routinely larger than the machine running this would like to hold.
func (a *App) exportSandboxTo(ctx context.Context, projectID, sandboxID, destination string, stdout, stderr io.Writer) error {
	resp, err := a.openSandboxExport(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	writer := stdout
	if destination != "-" {
		// O_EXCL: an export names its file after the discobox, so the obvious
		// second run of the same command would otherwise overwrite the first
		// one's archive without saying so.
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; name another file with -o", destination)
			}
			return err
		}
		defer file.Close()
		writer = file
		// Said before the copy, not after: an export is gigabytes, and a
		// redirect of stdout gets nothing without -o -, so the file this is
		// filling is otherwise invisible until it finishes.
		fmt.Fprintf(stderr, "Exporting to %s…\n", destination)
	}
	written, err := io.Copy(writer, resp.Body)
	if err != nil {
		return fmt.Errorf("export discobox: %w", err)
	}
	if destination != "-" {
		fmt.Fprintf(stderr, "Exported %s to %s\n", formatByteSize(written), destination)
	}
	return nil
}

// openSandboxExport starts the export and returns the response with its body
// still open; the caller closes it.
func (a *App) openSandboxExport(ctx context.Context, projectID, sandboxID string) (*http.Response, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	target, err := joinServerPath(baseURL, "/api/projects/"+url.PathEscape(projectID)+"/sandboxes/"+url.PathEscape(sandboxID)+"/export")
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", exportMediaType)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("export discobox: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, fmt.Errorf("export discobox: %s", responseMessage(resp))
	}
	return resp, nil
}

// importSandbox posts an archive to this invocation's server.
func (a *App) importSandbox(ctx context.Context, projectID string, archive io.Reader, opts sandboxImportOptions) (*sandboxImportResponse, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	target, err := joinServerPath(baseURL, "/api/projects/"+url.PathEscape(projectID)+"/sandboxes/import")
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if opts.name != "" {
		query.Set("name", opts.name)
	}
	if opts.poolID != "" {
		query.Set("pool", opts.poolID)
	}
	if opts.harnessSlug != "" {
		query.Set("harness", opts.harnessSlug)
	}
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, archive)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", exportMediaType)
	// The archive's length is not known when it is a stream from another
	// server, and net/http would otherwise buffer the whole thing to find out.
	req.ContentLength = -1
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("import discobox: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("import discobox: %s", responseMessage(resp))
	}
	var result sandboxImportResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("import discobox: %w", err)
	}
	if result.Sandbox == nil {
		return nil, errors.New("import discobox: the server returned no discobox")
	}
	return &result, nil
}

// transferSandbox streams one server's export straight into another's import.
//
// The two requests are joined by a pipe rather than a file: the archive is a
// whole workspace, and a transfer that needed room for a copy of it would fail
// on exactly the laptop most likely to be running this.
func (a *App) transferSandbox(cmd *cobra.Command, sandboxArg string, target *server, opts sandboxImportOptions, keep bool) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()
	projectID, sandboxID, client, err := a.sandboxRequest(ctx, sandboxArg)
	if err != nil {
		return err
	}
	if a.isSameServer(ctx, target) {
		return fmt.Errorf("%s is already on %s", sandboxArg, target.name)
	}
	sandbox, err := a.prepareSandboxForExport(ctx, client, projectID, sandboxID, true, stderr)
	if err != nil {
		return err
	}

	destinationProject, err := target.app.projectIDValue()
	if err != nil {
		return err
	}
	if opts.poolID != "" {
		targetClient, err := target.app.apiClient()
		if err != nil {
			return err
		}
		opts.poolID, err = target.app.resolvePoolID(ctx, targetClient, destinationProject, opts.poolID)
		if err != nil {
			return err
		}
	}

	export, err := a.openSandboxExport(ctx, projectID, sandboxID)
	if err != nil {
		return err
	}
	defer export.Body.Close()
	fmt.Fprintf(stderr, "Streaming %s to %s…\n", sandboxLabel(sandbox), target.name)
	result, err := target.app.importSandbox(ctx, destinationProject, export.Body, opts)
	if err != nil {
		return err
	}
	reportImportWarnings(stderr, result)

	if keep {
		fmt.Fprintf(stderr, "%s is still on %s, stopped\n", sandboxLabel(sandbox), a.sourceServerName())
	} else if err := archiveTransferredSandbox(ctx, client, projectID, sandboxID); err != nil {
		// The move landed; only the tidying up did not. Saying so plainly beats
		// failing a command whose work succeeded, and leaves the user one
		// command from the state they asked for.
		fmt.Fprintf(stderr, "Imported on %s, but %s could not be archived on %s: %v\n",
			target.name, sandboxLabel(sandbox), a.sourceServerName(), err)
	} else {
		fmt.Fprintf(stderr, "Archived %s on %s\n", sandboxLabel(sandbox), a.sourceServerName())
	}
	return a.writeSandbox(cmd, result.Sandbox)
}

// archiveTransferredSandbox archives the source. Delete is archive (ADR 0022
// §2), so this is the ordinary delete and the data is kept until the project's
// retention collects it.
func archiveTransferredSandbox(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) error {
	_, err := client.DeleteSandbox(ctx, apiclientgen.DeleteSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	return err
}

// isSameServer reports whether the transfer's destination is the server the
// discobox is already on.
//
// A registered server carries its peer ID; an address target has none until
// somebody asks, and an address is the spelling most likely to name the server
// you are on. Without asking, `--to <this server> --name other` stopped the
// discobox, copied it onto the same pool, and archived the original.
//
// A server that will not say who it is does not block the transfer: the guard
// is here to catch an obvious mistake, and an unanswered question is not one.
func (a *App) isSameServer(ctx context.Context, target *server) bool {
	if target == nil || target.app == nil {
		return false
	}
	if serverKey(a.serverURL) == serverKey(target.address) {
		return true
	}
	id := a.peerID(ctx)
	if id == "" {
		return false
	}
	if target.id == "" {
		target.id = target.app.peerID(ctx)
	}
	return samePeer(id, target.id)
}

// transferTarget resolves --to: a registered server's name, or an address.
func (a *App) transferTarget(to string) (*server, error) {
	value := strings.TrimSpace(to)
	if value == "" {
		return nil, errors.New("--to names the server to move the discobox to")
	}
	if strings.Contains(value, "://") {
		return &server{name: endpointLabel(value), address: value, app: a.forServer(value)}, nil
	}
	set, err := a.servers()
	if err != nil {
		return nil, err
	}
	target, ok := serverNamed(set, value)
	if !ok {
		return nil, fmt.Errorf("--to %q is neither an address nor a registered server; `discobox admin remote` lists the registered ones", value)
	}
	if target.app == nil {
		target.app = a.forServer(target.address)
	}
	return target, nil
}

// sourceServerName is what the server a transfer moves a discobox off is
// called in a progress line.
func (a *App) sourceServerName() string {
	if name := strings.TrimSpace(endpointLabel(a.serverURL)); name != "" {
		return name
	}
	return "this server"
}

// endpointLabel names a server in a transfer's own output, where two of them
// are on screen at once and telling them apart is the whole point.
//
// It is deliberately not addressLabel. That one is also what a registered name
// is derived from (sanitizeServerName), and a name may hold no colon, so it
// drops the port — which makes two servers on one host render identically.
// That is exactly the pair a transfer is most likely to be pointed at by
// mistake, and the lines it would blur are the two that say what became of
// somebody's data:
//
//	Streaming my-box to 127.0.0.1…
//	Archived my-box on 127.0.0.1
//
// A registered server is unaffected: its name is its own and already unique.
func endpointLabel(address string) string {
	label := addressLabel(address)
	parsed, err := endpoint.Parse(address)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return label
	}
	u, err := url.Parse(parsed.Value)
	if err != nil || u.Port() == "" {
		return label
	}
	return u.Host
}

func reportImportWarnings(stderr io.Writer, result *sandboxImportResponse) {
	for _, warning := range result.Warnings {
		fmt.Fprintf(stderr, "warning: %s\n", warning)
	}
}

// openImportArchive opens the archive to import: a file, or stdin for "-".
func openImportArchive(path string, stdin io.Reader) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	return file, func() { _ = file.Close() }, nil
}

func joinServerPath(baseURL, path string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	return parsed.String(), nil
}

// responseMessage is what a failed request said, preferring a structured error
// body over the bare status.
func responseMessage(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if message := attachErrorMessage(body); message != "" {
		return message
	}
	if trimmed := strings.TrimSpace(string(body)); trimmed != "" {
		return trimmed
	}
	return resp.Status
}

// formatByteSize is a size for a person watching a transfer, not for a machine.
func formatByteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value, exp := float64(n)/unit, 0
	for value >= unit && exp < 4 {
		value /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", value, "KMGTP"[exp])
}
