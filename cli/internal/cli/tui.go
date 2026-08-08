package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/sandboxcreate"
	"github.com/obot-platform/discobox/cli/internal/tui"
	"github.com/obot-platform/discobox/internal/gitutil"
)

// newTUICommand launches the interactive launcher: one full-screen window that
// opens with the cursor in a prompt for a new sandbox, with the project's
// sandboxes a press of Tab away.
func (a *App) newTUICommand() *cobra.Command {
	var leader string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive sandbox launcher",
		Long: `Launch the interactive launcher.

The window opens with the cursor in a prompt: type what the sandbox should do
and press Enter to run it in a new one, or press Enter on an empty prompt to
create one with nothing given to the harness. Tab moves to the list of
sandboxes you already have, where every action is a single letter — Enter
attaches, s opens a shell, d diffs, y applies, x archives — and Shift-Tab opens
"disco run"'s options.

Attaching or opening a shell draws the sandbox's terminal in the window itself.
Every key then goes to the sandbox except Ctrl-C, which detaches, and the ones
behind the leader: leader Ctrl-C sends a real interrupt through, and leader m
hands the mouse over or takes it back. The leader is Ctrl-A unless --leader or
DISCOBOX_LEADER says otherwise — worth changing when it collides with something
you run in your sandboxes.

The window takes the whole terminal while it is up, and puts back what was on
screen when it exits. Press F1 for the keys and Ctrl-C to quit.`,
		Example: `  disco tui
  disco tui --leader b
  DISCOBOX_LEADER=b disco tui`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			leaderKey, err := tui.NormalizeLeader(leader)
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			// The launcher owns the screen, so a pager it starts has to give it
			// back rather than leaving its last page on it for the window to
			// redraw over.
			a.pagerRestoresScreen = true
			ds := &apiDataSource{app: a, client: client, projectID: projectID}
			return tui.Run(cmd.Context(), ds, tui.WithLeader(leaderKey))
		},
	}
	cmd.Flags().StringVarP(&leader, "leader", "l", envOrDefault("DISCOBOX_LEADER", ""),
		"Prefix key for the terminal pane's own commands, as a single character taken as Ctrl-that (e.g. -l b for Ctrl-B); defaults to a")
	return cmd
}

// apiDataSource is the launcher's one seam onto the rest of the CLI. Everything
// the window does goes through here, and every action it runs is the command a
// shell would run: the interactive ones by building and executing the very same
// Cobra command, the rest through the same API client.
type apiDataSource struct {
	app       *App
	client    *apiclientgen.Client
	projectID string
}

// Session is what the header, the origin filter and the run options are drawn
// from: where the window is running and what the project offers.
func (d *apiDataSource) Session(ctx context.Context) (tui.Session, error) {
	session := tui.Session{
		Project:        d.projectID,
		DefaultProject: defaultProjectAlias,
	}

	origin, err := sandboxcreate.ResolveOrigin(ctx, d.app.source)
	if err != nil {
		return session, err
	}
	session.Directory = origin.ProjectPath
	if branch, ok := gitutil.CurrentBranch(ctx, origin.ProjectPath); ok {
		session.Branch = branch
	}

	// A project with no harness configs is a usable project — every sandbox in
	// it is a shell — so a listing that fails leaves the harness option empty
	// rather than refusing to open the window.
	if configs, err := d.app.listHarnessConfigs(ctx, d.client, d.projectID); err == nil {
		defaultID, _ := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
		for _, cfg := range configs {
			name := cfg.Slug
			if name == "" {
				name = cfg.Name
			}
			session.Harnesses = append(session.Harnesses, name)
			if cfg.ID == defaultID {
				session.DefaultHarness = name
			}
		}
	}
	return session, nil
}

// List is every sandbox in the project, most recently used first. The window
// filters to the ones started here itself, on a key, so the listing is not
// narrowed before it gets there.
func (d *apiDataSource) List(ctx context.Context) ([]tui.Sandbox, error) {
	res, err := d.client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: d.projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](res)
	if err != nil {
		return nil, err
	}
	sandboxes := sortedByRecency(body.GetSandboxes(), sandboxLastUsed)
	out := make([]tui.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, toTUISandbox(sb))
	}
	return out, nil
}

func toTUISandbox(sb apimodel.Sandbox) tui.Sandbox {
	row := tui.Sandbox{
		ID:       sb.ID,
		Name:     sb.Config.Name,
		State:    toTUIState(sandboxDisplayState(sb)),
		Message:  sandboxMessage(sb),
		LastUsed: sandboxLastUsed(sb),
	}
	if origin, ok := sb.Origin.Get(); ok {
		row.Folder = origin.ProjectPath
	}
	if cfg, ok := sb.HarnessConfig.Get(); ok {
		row.Harness = cfg.Slug
		if row.Harness == "" {
			row.Harness = cfg.Name
		}
	}
	if upgrade, ok := sb.Runtime.Upgrade.Get(); ok {
		row.Upgrade = upgrade.Available
	}
	if source, ok := sb.Config.Source.Get(); ok {
		if checkout, ok := source.Checkout.Get(); ok {
			row.Branch = strings.TrimSpace(checkout.RefName.Or(""))
			row.Commit = shortCommit(strings.TrimSpace(checkout.Commit.Or("")))
		}
		// A snapshot ref is the record of uncommitted work carried in at create,
		// which is exactly what the starred commit on the row means.
		row.Dirty = sourceSnapshotRef(source) != ""
	}
	return row
}

// toTUIState narrows the server's lifecycle states to the five the launcher
// draws. The transitional ones read as the state they are heading for, because
// that is the one that answers "can I act on this": a stopping sandbox takes
// the same actions a running one does until it is off.
func toTUIState(state string) tui.State {
	switch state {
	case "running":
		return tui.StateRunning
	case "starting":
		return tui.StateStarting
	case "stopping":
		return tui.StateRunning
	case "archived", "archiving":
		return tui.StateArchived
	case "error":
		return tui.StateError
	default:
		return tui.StateStopped
	}
}

// sandboxLastUsed is when the sandbox was last touched: the runtime's own
// activity timestamp, or the record's, which is what moved it up the list.
func sandboxLastUsed(sb apimodel.Sandbox) time.Time {
	if at, ok := sb.Runtime.LastActiveAt.Get(); ok && !at.IsZero() {
		return at
	}
	return recencyTime(sb.UpdatedAt, sb.CreatedAt)
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// DiffStat is what one sandbox has changed against the commit `disco diff`
// would diff it against, as `git diff --shortstat` reports it.
//
// It is tracked changes only. Untracked files are part of what `disco diff`
// shows, but finding them costs another round trip into the sandbox for a
// column that exists to rank rows, and a stat that sometimes waits twice as
// long is worse than one that is honest about counting commits and edits.
func (d *apiDataSource) DiffStat(ctx context.Context, sandboxID string) (tui.DiffStat, error) {
	res, err := d.client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return tui.DiffStat{}, err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return tui.DiffStat{}, err
	}
	sources, err := selectSources(sandbox, "")
	if err != nil || len(sources) == 0 {
		return tui.DiffStat{}, err
	}
	// The primary source is what the row is about. A sandbox with several is
	// rare, and a row cannot show a column per source anyway.
	entry := sources[0]
	workdir := sourceWorkdir(entry.source)
	base, _, err := d.app.resolveDiffBase(ctx, d.projectID, sandboxID, workdir, entry.source, "")
	if err != nil {
		return tui.DiffStat{}, err
	}
	stdout, stderr, code, err := d.app.sandboxCommandOutput(ctx, d.projectID, sandboxID, workdir,
		[]string{"git", "--no-pager", "diff", "--shortstat", base.Commit})
	if err != nil {
		return tui.DiffStat{}, err
	}
	if code != 0 {
		return tui.DiffStat{}, fmt.Errorf("git diff --shortstat: %s", strings.TrimSpace(stderr+stdout))
	}
	return parseShortstat(stdout), nil
}

// shortstatNumbers picks the three counts out of git's summary line, which
// reads "3 files changed, 61 insertions(+), 12 deletions(-)" — with any of the
// three clauses absent when it is zero.
var shortstatNumbers = regexp.MustCompile(`(\d+) (files? changed|insertions?\(\+\)|deletions?\(-\))`)

func parseShortstat(out string) tui.DiffStat {
	stat := tui.DiffStat{Known: true}
	for _, match := range shortstatNumbers.FindAllStringSubmatch(out, -1) {
		n, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(match[2], "file"):
			stat.Files = n
		case strings.HasPrefix(match[2], "insertion"):
			stat.Added = n
		case strings.HasPrefix(match[2], "deletion"):
			stat.Deleted = n
		}
	}
	return stat
}

// Dirty reports whether the source directory has uncommitted work, which is the
// question --include-dirty=auto exists to ask.
func (d *apiDataSource) Dirty(ctx context.Context, source string) (bool, error) {
	if strings.TrimSpace(source) == "" {
		source = d.app.source
	}
	// Outside a repository there is no uncommitted work to carry, and so no
	// question to ask about it.
	root, inRepo := gitRoot(ctx, sourceDirectory(source))
	if !inRepo {
		return false, nil
	}
	changes, err := gitutil.StatusChanges(ctx, root)
	if err != nil {
		return false, err
	}
	return len(changes) > 0, nil
}

// gitRoot is gitutil.Root asked as a question rather than for an error: not
// being in a repository is an answer here, not a failure, which is the same
// shape gitutil.CurrentBranch already takes for the same reason.
func gitRoot(ctx context.Context, dir string) (string, bool) {
	root, err := gitutil.Root(ctx, dir)
	return root, err == nil
}

// sourceDirectory is the directory half of a `-C DIR@REF` argument. The ref
// says which commit to cut from; the working tree being asked about is the
// directory's either way.
func sourceDirectory(source string) string {
	if i := strings.LastIndex(source, "@"); i > 0 {
		return source[:i]
	}
	if source == "" {
		return "."
	}
	return source
}

// Run creates the sandbox Enter asked for and delivers its source, which is
// exactly what `disco run` does before it attaches.
func (d *apiDataSource) Run(ctx context.Context, req tui.RunRequest) (tui.Sandbox, error) {
	opts := sandboxcreate.PromptOptions{
		Source:  strings.TrimSpace(req.Source),
		Harness: strings.TrimSpace(req.Harness),
		Env:     req.Env,
		Secret:  req.Secret,
	}
	if opts.Source == "" {
		opts.Source = d.app.source
	}
	// The window settles auto before it gets here, so the create never has to
	// ask a question there is nobody to answer.
	switch req.IncludeDirty {
	case "true":
		opts.IncludeDirty = sandboxcreate.IncludeDirtyAlways
	case "false":
		opts.IncludeDirty = sandboxcreate.IncludeDirtyNever
	}
	// The prompt goes in as the positional arguments, which is where the shared
	// parse takes it from: one argument, because the composer holds one piece of
	// text and splitting it would be inventing tokens the user did not type.
	var args []string
	if req.Prompt != "" {
		args = []string{req.Prompt}
	}
	opts, err := sandboxcreate.ParsePromptOptions(opts, args)
	if err != nil {
		return tui.Sandbox{}, err
	}

	sandbox, err := sandboxcreate.CreatePromptSandbox(ctx, d.client, d.projectID, opts)
	if err != nil {
		return tui.Sandbox{}, err
	}
	// A server that cannot reach this directory waits for us to push it.
	gitServerURL, releaseGitServerURL, err := d.app.gitServerURL(ctx)
	if err != nil {
		return tui.Sandbox{}, err
	}
	err = sandboxcreate.DeliverSource(ctx, d.client, d.projectID, sandbox, opts.Source, gitServerURL, d.app.token)
	releaseGitServerURL()
	if err != nil {
		return tui.Sandbox{}, err
	}
	return toTUISandbox(*sandbox), nil
}

// Do runs a lifecycle verb against one sandbox. These go straight to the API
// rather than through their Cobra commands: the commands print a sandbox table
// on success, and the window reports on its own status line instead.
func (d *apiDataSource) Do(ctx context.Context, verb tui.Verb, sandboxID string) error {
	switch verb {
	case tui.VerbStart:
		res, err := d.client.StartSandbox(ctx, &apimodel.StartSandboxBody{},
			apiclientgen.StartSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		_, err = expectResponse[apimodel.Sandbox](res)
		return err

	case tui.VerbStop:
		res, err := d.client.StopSandbox(ctx, &apimodel.StopSandboxBody{},
			apiclientgen.StopSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		_, err = expectResponse[apimodel.Sandbox](res)
		return err

	case tui.VerbUpgrade:
		res, err := d.client.UpgradeSandbox(ctx, &apimodel.UpgradeSandboxBody{},
			apiclientgen.UpgradeSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		_, err = expectResponse[apimodel.Sandbox](res)
		return err

	case tui.VerbArchive:
		res, err := d.client.DeleteSandbox(ctx, apiclientgen.DeleteSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		return expectNoContent[apiclientgen.DeleteSandboxAccepted](res)

	case tui.VerbUnarchive:
		res, err := d.client.UnarchiveSandbox(ctx, apiclientgen.UnarchiveSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		return expectNoContent[apiclientgen.UnarchiveSandboxAccepted](res)

	case tui.VerbPurge:
		res, err := d.client.PurgeSandbox(ctx, apiclientgen.PurgeSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		return expectNoContent[apiclientgen.PurgeSandboxNoContent](res)

	default:
		return fmt.Errorf("unknown action %q", verb)
	}
}

// Interact runs one of the command-shaped actions with the real terminal's
// streams, while the window is suspended.
//
// It builds and executes the actual Cobra command rather than calling into its
// internals, so what the launcher runs is `disco diff`, `disco apply` and
// `disco status` — with their own flag defaults, their pager, their terminal
// detection — and not a second implementation that drifts from them.
//
// The two actions that are terminals rather than commands, attach and shell,
// are drawn in the window instead; see Open.
func (d *apiDataSource) Interact(ctx context.Context, action tui.Interaction, sandboxIDs []string, stdin io.Reader, stdout, stderr io.Writer) error {
	for i, id := range sandboxIDs {
		var cmd *cobra.Command
		switch action {
		case tui.InteractDiff:
			cmd = d.app.newDiffCommand()
		case tui.InteractApply:
			cmd = d.app.newApplyCommand()
		case tui.InteractStatus:
			cmd = d.app.newStatusCommand()
		default:
			// Attach and shell are terminals the window draws itself; they go
			// through Open, not through a command that wants the real terminal.
			return fmt.Errorf("%s is not a command", action)
		}
		// Several sandboxes in one run need telling apart, and only the ones
		// that print do: attach and shell take exactly one.
		if len(sandboxIDs) > 1 {
			if i > 0 {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stderr, "── %s\n", id)
		}
		cmd.SetContext(ctx)
		cmd.SetArgs([]string{id})
		cmd.SetIn(stdin)
		cmd.SetOut(stdout)
		cmd.SetErr(stderr)
		// The window reports the failure on its status line, so the command
		// neither prints it again nor follows it with a page of usage.
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		if err := cmd.Execute(); err != nil {
			return err
		}
	}
	return nil
}
