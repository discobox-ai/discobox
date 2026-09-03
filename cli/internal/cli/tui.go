package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/keys"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/x/gitutil"
)

// newTUICommand launches the interactive launcher: one full-screen window that
// opens with the cursor in a prompt for a new sandbox, with the project's
// sandboxes a press of Tab away.
func (a *App) newTUICommand() *cobra.Command {
	var leaderFlag string
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive discobox launcher",
		Long: `Launch the interactive launcher.

The window opens with the cursor in a prompt: type what the discobox should do
and press Enter to run it in a new one, or press Enter on an empty prompt to
create one with nothing given to the harness. Tab moves to the list of
discoboxes you already have, where every action is a single letter — Enter
attaches, s opens a shell, y applies, x archives — and Shift-Tab opens
"discobox run"'s options.

Attaching or opening a shell draws the discobox's terminal in the window itself.
Every key then goes to the discobox, Ctrl-C included: it is the program's, so
what you interrupt is what is running rather than the window around it. The
window's own keys are behind the leader — leader q detaches, leader m hands the
mouse over or takes it back — and the leader is Ctrl-A unless --leader or
DISCOBOX_LEADER says otherwise, worth changing when it collides with something
you run in your discoboxes. It is the same leader "discobox attach" detaches behind.

F3 opens the harnesses — the same screen "discobox configure" opens — where an
harness is enabled, disabled, made the project default, or has its files edited.
What is enabled there is what the run options offer as the harness to run.

The window takes the whole terminal while it is up, and puts back what was on
screen when it exits. Press F1 for the keys, and Ctrl-C to quit once no
discobox terminal is up.`,
		Example: `  discobox tui
  discobox tui --leader b
  DISCOBOX_LEADER=b discobox tui`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runTUI(cmd, leaderFlag)
		},
	}
	cmd.Flags().StringVarP(&leaderFlag, "leader", "l", "",
		"Prefix key for the terminal pane's own commands, as a single letter taken as Ctrl-that (e.g. -l b for Ctrl-B); defaults to "+keys.LeaderEnv+", then a")
	return cmd
}

// runTUI starts the launcher. It is reached five ways — `discobox tui`, `discobox`
// with nothing to do, `discobox configure`, which is the launcher opened on its
// harnesses screen, `discobox run`, which is the launcher opened on one run
// (`tui.WithRun`), and `discobox attach`, which is it opened on one discobox
// (`tui.WithAttach`) — so it lives here rather than inside any one's RunE.
//
// leaderFlag is --leader, empty when it was not given: the environment's leader
// is already resolved on the App, and only an explicit flag displaces it.
func (a *App) runTUI(cmd *cobra.Command, leaderFlag string, options ...tui.Option) error {
	leaderKey := a.leader()
	if leaderFlag != "" {
		var err error
		if leaderKey, err = keys.NormalizeLeader(leaderFlag); err != nil {
			return err
		}
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	// The window reports first-run setup itself, under its own frame, so the
	// launch below must not sit on it: several minutes of a status line before
	// anything appears is the thing this replaces. Nothing the window does
	// needs those images — only running a discobox does.
	a.stagingShownByUI = true
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	ds := &apiDataSource{app: a, client: client, projectID: projectID}
	options = append([]tui.Option{tui.WithLeader(leaderKey)}, options...)
	// Whether to introduce Discobox is settled before the window opens rather
	// than when the session load comes back: a welcome that arrives a moment
	// later would arrive over a prompt the user is already typing into, and the
	// first run — the one this is for — is the slowest load there is. Whether
	// it is actually shown from here is the window's own call — see
	// tui.WithAttach and tui.WithRun — since a window opened on one discobox
	// (`discobox attach`) has no screen behind the introduction to hand over
	// to, while one opened on one run (`discobox run`) does.
	welcomed, err := a.projectWelcomed(cmd.Context(), client, projectID)
	if err != nil {
		return err
	}
	if !welcomed {
		options = append(options, tui.WithWelcome())
	}
	if a.startedServer {
		options = append(options, tui.WithInitialization("Server initialization", a.stagingUpdates(cmd.Context())))
	}
	return tui.Run(cmd.Context(), ds, options...)
}

// canOpenWindow reports whether this invocation can put a full-screen window
// on the terminal it was run from, which is what `discobox run` and `discobox
// attach` do unless --raw says otherwise. It is the rule bare `discobox`
// already uses: a pipe, a script, or CI has no terminal to draw one on, and
// there the attach is the byte stream it always was.
func canOpenWindow(cmd *cobra.Command) bool {
	return isTerminalStream(cmd.InOrStdin()) && isTerminalStream(cmd.OutOrStdout())
}

// projectWelcomed reports whether this project has already shown its
// introduction. A project the server cannot describe is an error worth stopping
// on: everything the window does needs the project, and a launcher that opened
// anyway would fail again on its first request.
func (a *App) projectWelcomed(ctx context.Context, client *apiclientgen.Client, projectID string) (bool, error) {
	project, err := a.getProject(ctx, client, projectID)
	if err != nil {
		return false, err
	}
	return project.Welcomed, nil
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
	// Whatever was left unsent here last time. It is local state, not the
	// project's: a draft belongs to the checkout it was written in and to the
	// machine it was written on. See drafts.go.
	session.Draft = promptDraftFor(session.Directory)
	// The harnesses are not here: they are read on their own by Harnesses,
	// which is what both the run options and the harnesses screen are drawn
	// from. See tui_harnesses.go.
	return session, nil
}

// MarkWelcomed records on the project that its introduction has been shown. It
// is a project setting like any other, so it goes through the same update the
// `discobox project` commands use rather than an endpoint of its own.
func (d *apiDataSource) MarkWelcomed(ctx context.Context) error {
	body := &apimodel.UpdateProjectBody{Welcomed: apiclientgen.NewOptBool(true)}
	res, err := d.client.UpdateProject(ctx, body, apiclientgen.UpdateProjectParams{ProjectId: d.projectID})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Project](res)
	return err
}

// SaveDraft keeps the composer's contents against the folder they were typed
// in, for the next window that opens on it.
//
// The context is not used and is not wanted: this is a local file, and the
// window's last call is the one it makes on the way out, when the context it
// is holding may already be on its way down with the program.
func (d *apiDataSource) SaveDraft(_ context.Context, folder, prompt string) error {
	return savePromptDraft(folder, prompt)
}

// List is every sandbox in the project, newest-created first — the same
// order `discobox ls` prints, for the same reason: creation is the one
// timestamp a user's action put there. The window
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
	sandboxes := sortedByRecency(body.GetSandboxes(), func(sb apimodel.Sandbox) time.Time { return sb.CreatedAt })
	out := make([]tui.Sandbox, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, toTUISandbox(sb))
	}
	return out, nil
}

// Resources is what Discobox has on this machine and what it is using of it.
//
// It reads the pools rather than the discoboxes, because it is true with none:
// somebody deciding whether to start their first one still wants to know what
// room there is. The agent already added the two halves — its own services and
// every discobox — at one tick, so nothing is summed across rows here.
//
// Capacity is what is dedicated to Discobox. The agent can only measure the
// host, so the envelope an operator set on the pool is preferred here, where
// both numbers are known; an envelope of zero means "sized by the host".
func (d *apiDataSource) Resources(ctx context.Context) (tui.Resources, error) {
	res, err := d.client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: d.projectID})
	if err != nil {
		return tui.Resources{}, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return tui.Resources{}, err
	}
	var out tui.Resources
	for _, pool := range body.GetPools() {
		report, ok := poolResourceReport(&pool)
		if !ok {
			continue
		}
		total, ok := report.Total.Get()
		if !ok {
			continue
		}
		// Summed across pools, because what somebody wants to know is what
		// Discobox has in total. A project with one pool — which is every
		// project today — sums one.
		out.CPUCapacity += dedicated(pool.CpuVcpus, total.CapacityVcpus)
		out.MemoryCapacity += int64(dedicated(float64(pool.MemoryBytes), float64(total.CapacityBytes)))
		out.MemoryBytes += total.MemoryBytes
		if vcpus, ok := total.CpuVcpus.Get(); ok {
			out.CPUVCPUs += vcpus
			out.Known = true
		}
		if free := report.Storage.Filesystem.FreeBytes; free > 0 {
			out.DiskFreeBytes += free
			out.DiskKnown = true
		}
		// The walk lags the statfs above, so free space can be known before the
		// breakdown of what is taken is. The breakdown is added only once it
		// exists rather than being carried as zeroes.
		if walk, ok := report.Storage.Walk.Get(); ok {
			out.DiskDataBytes += walk.DataBytes.Or(0)
			// The builder's store is disposable and shared, like the cache
			// beside it, and neither belongs to any one discobox — so a reader
			// deciding what could be reclaimed reads them as one figure.
			out.DiskCacheBytes += walk.CacheBytes + walk.BuildBytes
		}
	}
	return out, nil
}

// dedicated prefers the envelope an operator set over the host's own capacity.
// Zero is not a smaller envelope, it is the absence of one (model.PoolManifest).
func dedicated(envelope, capacity float64) float64 {
	if envelope > 0 {
		return envelope
	}
	return capacity
}

// toTUIUsage is what the row's usage column draws: the sandbox's share of the
// host it runs on, from the pool agent's resource report (ADR 0071).
//
// The denominators come from the sandbox's own pool, which the listing already
// carries, so drawing the column costs no extra request. They are the host's
// capacity rather than the pool's envelope because the envelope is usually
// zero, meaning "sized by the host" — a share of zero is not a share.
//
// Everything here is absent-or-measured, never defaulted. A share needs both a
// measured figure and a denominator to divide it by, and when either is missing
// the row draws a dot: a zero share would say the sandbox is idle, which is a
// claim about it rather than about what we know.
func toTUIUsage(sb apimodel.Sandbox) tui.Usage {
	consumption, ok := sandboxResourceConsumption(sb)
	if !ok {
		return tui.Usage{}
	}
	var usage tui.Usage
	pool, hasPool := sb.Pool.Get()
	var report apimodel.PoolResourceReport
	if hasPool {
		report, hasPool = poolResourceReport(&pool)
	}

	// CPU and memory arrive together, so one flag covers both — but only when
	// the rate itself was measured. The first report after an agent starts
	// carries counters and no rate at all.
	if cpu, ok := consumption.CPU.Get(); ok && hasPool {
		if vcpus, ok := cpu.Vcpus.Get(); ok {
			if capacity := report.CPU.CapacityVcpus.Or(0); capacity > 0 {
				usage.CPUPercent = percentOf(vcpus, capacity)
				usage.Known = true
			}
		}
	}
	// Resident rather than the cgroup's charge, and rather than virtual size.
	// Virtual is address space reserved, not memory held, so it tells one
	// discobox's weight from another's not at all; the charge includes
	// reclaimable page cache, which is the right thing to read against the
	// machine's capacity and the wrong thing to read against another discobox.
	if memory, ok := consumption.Memory.Get(); ok && usage.Known {
		usage.MemoryBytes = memory.ResidentBytes
		if capacity := report.Memory.CapacityBytes.Or(0); capacity > 0 {
			usage.MemoryPercent = percentOf(float64(memory.ResidentBytes), float64(capacity))
		}
	}
	// Disk is walked on the agent's own slower schedule, so it is known
	// separately and lags the counters on a newly created sandbox.
	if storage, ok := consumption.Storage.Get(); ok {
		usage.DiskBytes = storage.TotalBytes
		usage.DiskKnown = true
		if hasPool {
			if total := report.Storage.Filesystem.TotalBytes; total > 0 {
				usage.DiskPercent = percentOf(float64(storage.TotalBytes), float64(total))
			}
		}
	}
	return usage
}

// percentOf rounds a share to whole percent and clamps it to 0..100. A rate
// sampled a moment apart from its denominator can exceed it slightly, and a
// row reading "103%" is a measurement artifact rather than a finding.
func percentOf(value, of float64) int {
	if of <= 0 {
		return 0
	}
	return min(max(int(value/of*100+0.5), 0), 100)
}

func toTUISandbox(sb apimodel.Sandbox) tui.Sandbox {
	row := tui.Sandbox{
		ID:   sb.ID,
		Name: sb.DisplayName,
		// The name the box is configured with, beside the name the server
		// says to show — which is the primary terminal's title once there is
		// one, and the id when there is no name at all. Trimmed the way the
		// server trims it before falling back, so blank and unset are the
		// same answer on both ends.
		ConfigName: strings.TrimSpace(sb.Config.Name),
		State:      toTUIState(sandboxDisplayState(sb)),
		// The presence of runtimeState is the signal, not its value: the API
		// omits the field entirely until the pool agent has reported on this
		// sandbox, and empty is not `stopped` (ADR 0034 §2). displayState folds
		// that axis away — an errored box reads `error` whatever its container
		// is doing — so the row carries it separately for the guards.
		HasRuntime: sb.Runtime.RuntimeState.IsSet(),
		Message:    sandboxMessage(sb),
		Created:    sb.CreatedAt,
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
	row.Usage = toTUIUsage(sb)
	if source, ok := sb.Config.Source.Get(); ok {
		// What the run options offer to cut a new discobox from, spelled the
		// way `-C` takes it. A remote source is the URL the discobox clones; a
		// local one is the client directory it was cut from, which is a path on
		// the machine that created it.
		if url, ok := source.URL.Get(); ok {
			row.Source, row.SourceRemote = url.String(), true
		} else {
			row.Source = strings.TrimSpace(source.LocalDirectory.Or(""))
		}
		if checkout, ok := source.Checkout.Get(); ok {
			row.Branch = strings.TrimSpace(checkout.RefName.Or(""))
			row.Commit = shortCommit(strings.TrimSpace(checkout.Commit.Or("")))
		}
		// A snapshot ref is the record of uncommitted work carried in at create,
		// which is exactly what the starred commit on the row means.
		row.Dirty = sourceSnapshotRef(source) != ""
	}
	row.Ports = sandboxListeningPorts(sb)
	if git := sandboxGitStatus(sb); git.Known {
		row.Git = tui.GitState{
			Known:         true,
			Branch:        git.Branch,
			Commit:        shortCommit(git.Commit),
			Dirty:         !git.Clean,
			Applied:       git.Applied,
			AppliedCommit: shortCommit(git.AppliedHostCommit),
		}
		if git.DiffKnown {
			row.Diff = tui.DiffStat{
				Known:   true,
				Files:   git.DiffFiles,
				Added:   git.DiffAdded,
				Deleted: git.DiffDeleted,
			}
		}
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

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// Dirty reports whether the source directory has uncommitted work, which is the
// question --include-dirty=auto exists to ask.
func (d *apiDataSource) Workspace(ctx context.Context, source string) (tui.SourceWorkspace, error) {
	if strings.TrimSpace(source) == "" {
		source = d.app.source
	}
	// A repository the discobox clones for itself has no working tree here, so
	// there is nothing it could carry and nothing to ask about.
	if sandboxcreate.IsRemoteGitSource(sourceDirectory(source)) {
		return tui.SourceWorkspace{Directory: source}, nil
	}
	dir, err := filepath.Abs(sourceDirectory(source))
	if err != nil {
		return tui.SourceWorkspace{}, err
	}
	// Outside a repository there is no commit to fall back to, so what the
	// directory carries is the whole of it — and an empty one carries nothing,
	// which is the same discobox either answer would have produced.
	root, inRepo := gitRoot(ctx, dir)
	if !inRepo {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return tui.SourceWorkspace{}, err
		}
		return tui.SourceWorkspace{Directory: dir, Carries: len(entries) > 0}, nil
	}
	changes, err := gitutil.StatusChanges(ctx, root)
	if err != nil {
		return tui.SourceWorkspace{}, err
	}
	// The paths as well as the count: the question about them names a few, so
	// what is being carried in is recognizable rather than a number.
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.Path)
	}
	return tui.SourceWorkspace{Directory: root, Repository: true, Carries: len(paths) > 0, Changes: paths}, nil
}

// MeasureDirectory is the shared create path's directory walk, in the shape the
// window polls it in. The two frontends count the same thing the same way; only
// where the number is drawn differs.
func (d *apiDataSource) MeasureDirectory(ctx context.Context, dir string) (func() tui.DirectoryTotal, func()) {
	walk := sandboxcreate.MeasureDirectory(ctx, dir)
	return func() tui.DirectoryTotal { return tui.DirectoryTotal(walk.Total()) }, walk.Stop
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
// exactly what `discobox run` does before it attaches — including saying which of
// those steps is underway, on the same words the command uses (ADR 0060).
func (d *apiDataSource) Run(ctx context.Context, req tui.RunRequest, report func(string)) (tui.Sandbox, error) {
	opts := sandboxcreate.PromptOptions{
		Source:   strings.TrimSpace(req.Source),
		NoSource: req.NoSource,
		Harness:  strings.TrimSpace(req.Harness),
		Env:      req.Env,
		Secret:   req.Secret,
		// Both only ever come from `discobox run`'s own request, which the
		// window is opened on: the panel offers no way to name an extra source
		// or to leave the declared ones out. See tui.WithRun.
		Include:             req.Include,
		SkipDeclaredSources: req.SkipDeclaredSources,
	}
	// What each declared source resolved to, in the window's own form: it has
	// no scrollback to keep a line in, so the report is narrated as it happens
	// beside the step that is making it.
	if report != nil {
		opts.ReportDeclaredSource = func(source sandboxcreate.DeclaredSource) {
			report(declaredSourceLine(source))
		}
	}
	// With no source there is no directory to cut from, and -C says only where
	// the create came from — which for the window is where it is running, the
	// same as an unset source.
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
	// parse takes it from.
	opts, err := sandboxcreate.ParsePromptOptions(opts, req.Prompt)
	if err != nil {
		return tui.Sandbox{}, err
	}

	step := func(step sandboxcreate.Step) {
		if report != nil {
			report(string(step))
		}
	}
	sandbox, local, err := sandboxcreate.CreatePromptSandbox(ctx, d.client, d.projectID, opts, step)
	if err != nil {
		return tui.Sandbox{}, err
	}
	// The local source is done as soon as it has been delivered, which for a
	// directory that is not a repository means deleting the repository built
	// over it. The defer covers the paths that never reach the delivery.
	defer local.Close()
	// A server that cannot reach this directory waits for us to push it.
	gitServerURL, releaseGitServerURL, err := d.app.gitServerURL(ctx)
	if err != nil {
		return tui.Sandbox{}, err
	}
	err = sandboxcreate.DeliverSource(ctx, d.client, d.projectID, sandbox, local, gitServerURL, d.app.token, step)
	releaseGitServerURL()
	local.Close()
	if err != nil {
		return tui.Sandbox{}, err
	}
	if report != nil {
		report("syncing SSH config")
	}
	// Which key it enrolled and which files it wrote go on the busy line with
	// everything else this create says. Nothing reaches the terminal: the
	// window is a full-screen program on it, and a line written past the
	// renderer draws over the frame — which is what "wrote …/config" used to
	// do, on the stream this used to hand it.
	notes := noteFunc(func(format string, args ...any) {
		if report != nil {
			report(fmt.Sprintf(format, args...))
		}
	})
	if err := d.app.writeProjectSSHConfig(ctx, d.client, d.projectID, "", notes); err != nil {
		return tui.Sandbox{}, fmt.Errorf("sync SSH config: %w", err)
	}
	return toTUISandbox(*sandbox), nil
}

// WatchProvisioning says what a discobox that is not usable yet is being made
// to do, on the same reading of the same record `discobox run` narrates from.
func (d *apiDataSource) WatchProvisioning(ctx context.Context, sandboxID string, report func(string)) {
	d.app.watchProvisioning(ctx, d.projectID, sandboxID, report)
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

	case tui.VerbRepair:
		// Repair converges in the request (ADR 0035), so this call is as long
		// as the rebuild is — the window shows its busy line meanwhile.
		res, err := d.client.RepairSandbox(ctx, apiclientgen.RepairSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
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

// Rename gives a sandbox a new name — the one piece of its config the window
// edits, through the same PATCH `discobox admin box update --name` uses.
func (d *apiDataSource) Rename(ctx context.Context, sandboxID, name string) error {
	body := &apimodel.UpdateSandboxBody{}
	body.SetConfig(apiclientgen.NewOptSandboxUpdateConfig(apimodel.SandboxUpdateConfig{
		Name: apiclientgen.NewOptString(name),
	}))
	res, err := d.client.UpdateSandbox(ctx, body, apiclientgen.UpdateSandboxParams{ProjectId: d.projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Sandbox](res)
	return err
}

// OpenEditor opens one sandbox in VS Code, by running `discobox tools vscode`.
//
// It runs the command rather than reimplementing it for the same reason an
// overlay pane runs `discobox apply`: what the window opens is the command, with
// its own editor discovery and its own ssh_config handling, not a second
// version of them.
//
// Nothing it writes reaches the screen. The window is a full-screen program
// that a stray line of stderr would draw over, and the command's progress
// reporting — which key it enrolled, which config it wrote — is not what
// someone pressing a key in a list is waiting to read. What went wrong still
// comes back as the error, which the status line reports.
func (d *apiDataSource) OpenEditor(ctx context.Context, sandboxID string) error {
	sandboxFlag := ""
	cmd := d.app.newToolsVSCodeCommand(&sandboxFlag)
	cmd.SetContext(ctx)
	cmd.SetArgs([]string{sandboxID})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	return cmd.Execute()
}
