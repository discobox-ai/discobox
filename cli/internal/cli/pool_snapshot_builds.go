package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A pool's snapshot builds, reached through the pool (ADR 26-11-23-004 §§4,
// 7): listed beside the boxes from each pool's report, their feature logs
// read, and a shell opened in their box. A build is the pool's: the server
// keeps nothing of it, and what its progress says is the build's own word,
// escaped as it is shown (REVIEW.md).

func (a *App) newPoolSnapshotCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Reach a pool's snapshot builds",
		Long: `Reach the snapshot builds a pool runs: the boxes it provisions for no
discobox, so that a new discobox of the same plan starts from one instead of
provisioning from scratch.

"discobox admin pool get POOL" lists them with their keys; "discobox admin box
ls --snapshot-builds" lists them beside the boxes. A KEY may be the short key
those tables show.`,
	}
	cmd.AddCommand(a.newPoolSnapshotLogsCommand())
	cmd.AddCommand(a.newPoolSnapshotExecCommand())
	return cmd
}

func (a *App) newPoolSnapshotLogsCommand() *cobra.Command {
	var feature string
	var tail int
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs POOL KEY",
		Short: "Print a snapshot build's feature logs",
		Long: `Print the feature logs of one of a pool's snapshot builds: what each
feature's steps wrote as the build provisioned it.

The pool reads them through the build's own box while it runs, on every
runtime, so no provider credential or console is needed. A build that failed
recently keeps the end of each log, which this prints after its box is gone.`,
		Example: `  discobox admin pool snapshot logs pool_01hq 3f9a2c1d0e4b
  discobox admin pool snapshot logs pool_01hq 3f9a2c1d0e4b --feature docker -f`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, poolID, key, err := a.snapshotBuildTarget(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			return a.streamSnapshotBuildLogs(cmd.Context(), projectID, poolID, key, feature, tail, follow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&feature, "feature", "", "Print one feature's log (default every feature's)")
	cmd.Flags().IntVar(&tail, "tail", defaultPoolLogTail, "Print only the last N lines of each log (0 for the whole log)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Keep printing until the build ends")
	return cmd
}

func (a *App) newPoolSnapshotExecCommand() *cobra.Command {
	var forceTTY bool
	cmd := &cobra.Command{
		Use:   "exec POOL KEY [--] [CMD...]",
		Short: "Open a shell in a snapshot build's box",
		Long: `Open a shell, or run a command, in the box of one of a pool's running
snapshot builds, to see why it is slow or stuck.

The pool reaches the build's box itself, on every runtime, so no provider
credential or console is needed. The build runs on while you look, but once you
have run anything in its box its result is never kept as a snapshot: whatever
you changed would be in every box made from it. The next discobox of its plan
asks for the build again.

The session ends when the build does, since the build removes its box.
Everything after KEY is the command; an optional -- separates them. A TTY is
allocated when stdin is a terminal, or with -t.`,
		Example: `  discobox admin pool snapshot exec pool_01hq 3f9a2c1d0e4b
  discobox admin pool snapshot exec pool_01hq 3f9a2c1d0e4b -- ps aux`,
		Args:              cobra.MinimumNArgs(2),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, poolID, key, err := a.snapshotBuildTarget(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			command := args[2:]
			if len(command) > 0 && command[0] == "--" {
				command = command[1:]
			}
			route := poolExecRoute{
				path: snapshotBuildPath(projectID, poolID, key, "exec"),
				kind: "snapshot build", where: "the build's box",
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Build %s on pool %s will not be kept as a snapshot once anything runs in its box.\n", shortSnapshotKey(key), poolID)
			if len(command) > 0 {
				tty := forceTTY || isTerminalStream(cmd.InOrStdin())
				return a.attachPoolExec(cmd.Context(), route, command, tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Opening a shell in build %s's box (%s to leave)\n", shortSnapshotKey(key), a.detachHint())
			return a.attachPoolExec(cmd.Context(), route, nil, true, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVarP(&forceTTY, "tty", "t", false, "Allocate a TTY for a command")
	return cmd
}

// snapshotBuildPath is the server's route for the build keyed key.
func snapshotBuildPath(projectID, poolID, key, route string) string {
	return "/projects/" + url.PathEscape(projectID) + "/pools/" + url.PathEscape(poolID) + "/snapshot-builds/" + url.PathEscape(key) + "/" + route
}

// snapshotBuildTarget resolves POOL and KEY: the pool as every pool command
// does, and the key, whole or the start of one, against the builds and
// failed builds the pool reports.
func (a *App) snapshotBuildTarget(ctx context.Context, poolArg, keyArg string) (projectID, poolID, key string, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return "", "", "", err
	}
	client, err := a.apiClient()
	if err != nil {
		return "", "", "", err
	}
	poolID, err = a.resolvePoolID(ctx, client, projectID, poolArg)
	if err != nil {
		return "", "", "", err
	}
	res, err := client.GetPool(ctx, apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
	if err != nil {
		return "", "", "", err
	}
	pool, err := expectResponse[apimodel.Pool](res)
	if err != nil {
		return "", "", "", err
	}
	_, snapshots := poolSnapshotsSummary(*pool)
	if snapshots == nil {
		return "", "", "", fmt.Errorf("pool %s has not reported its snapshot builds", poolID)
	}
	keys := map[string]bool{}
	for _, build := range snapshots.Building {
		keys[build.Key] = true
	}
	for _, failed := range snapshots.Failed {
		keys[failed.Key] = true
	}
	key, err = resolveSnapshotBuildKey(strings.TrimSpace(keyArg), slices.Sorted(maps.Keys(keys)))
	if err != nil {
		return "", "", "", fmt.Errorf("pool %s: %w", poolID, err)
	}
	return projectID, poolID, key, nil
}

// resolveSnapshotBuildKey is the one of keys that value is, or begins.
func resolveSnapshotBuildKey(value string, keys []string) (string, error) {
	if value == "" {
		return "", errors.New("no snapshot build key given")
	}
	var matches []string
	for _, key := range keys {
		if key == value {
			return key, nil
		}
		if strings.HasPrefix(key, value) {
			matches = append(matches, key)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no snapshot build under way or failed recently has key %q", value)
	case 1:
		return matches[0], nil
	}
	short := make([]string, len(matches))
	for i, match := range matches {
		short[i] = shortSnapshotKey(match)
	}
	return "", fmt.Errorf("snapshot build key %q is ambiguous; matches %s", value, strings.Join(short, ", "))
}

// streamSnapshotBuildLogs copies a build's feature logs to stdout: the last
// tail lines of each, or all of them for zero.
func (a *App) streamSnapshotBuildLogs(ctx context.Context, projectID, poolID, key, feature string, tail int, follow bool, stdout io.Writer) error {
	query := url.Values{}
	if feature != "" {
		query.Set("feature", feature)
	}
	if tail > 0 {
		query.Set("tail", strconv.Itoa(tail))
	}
	if follow {
		query.Set("follow", "true")
	}
	resp, err := a.openPoolText(ctx, snapshotBuildPath(projectID, poolID, key, "logs"), query, "read the snapshot build's logs")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// What the build's steps wrote is the build's: shown escaped, as its
	// progress is.
	_, err = io.Copy(&terminalSafeWriter{w: stdout}, resp.Body)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("read the snapshot build's logs: %w", err)
	}
	return nil
}

// terminalSafeWriter writes what is written to it escaped for the terminal,
// line breaks and tabs kept, as it comes: a download's progress redrawn on a
// carriage return shows as it is written, and nothing is held but a rune cut
// in two by a read.
type terminalSafeWriter struct {
	w       io.Writer
	partial []byte
}

func (t *terminalSafeWriter) Write(p []byte) (int, error) {
	data := append(t.partial, p...)
	end := len(data)
	// Hold back a rune still arriving, at most utf8.UTFMax-1 bytes of it.
	for i := 1; i < utf8.UTFMax && i <= len(data); i++ {
		if r := data[len(data)-i]; utf8.RuneStart(r) {
			if !utf8.FullRune(data[len(data)-i:]) {
				end = len(data) - i
			}
			break
		}
	}
	t.partial = append([]byte(nil), data[end:]...)
	if _, err := io.WriteString(t.w, terminalSafeMultiline(string(data[:end]))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// poolSnapshotBuild is one build as `admin box ls --snapshot-builds` lists
// it: the pool's report entry, with the pool it runs on.
type poolSnapshotBuild struct {
	Pool  string                     `json:"pool"`
	Build apimodel.PoolSnapshotBuild `json:"build"`
}

// listSnapshotBuilds is every build the project's pools report, by pool.
func (a *App) listSnapshotBuilds(ctx context.Context, client *apiclientgen.Client, projectID string) ([]poolSnapshotBuild, error) {
	res, err := client.ListPools(ctx, apiclientgen.ListPoolsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListPoolsBody](res)
	if err != nil {
		return nil, err
	}
	builds := []poolSnapshotBuild{}
	for _, pool := range body.GetPools() {
		_, snapshots := poolSnapshotsSummary(pool)
		if snapshots == nil {
			continue
		}
		for _, build := range snapshots.Building {
			builds = append(builds, poolSnapshotBuild{Pool: pool.ID, Build: build})
		}
	}
	slices.SortFunc(builds, func(x, y poolSnapshotBuild) int { return x.Build.RequestedAt.Compare(y.Build.RequestedAt) })
	return builds, nil
}

// writeSnapshotBuilds writes one row per build: its pool and short key, its
// state, when, its box, what it is provisioning, and its plan.
func writeSnapshotBuilds(w io.Writer, builds []poolSnapshotBuild, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POOL\tSNAPSHOT\tSTATE\tWHEN\tBOX\tPROGRESS\tPLAN")
	for _, entry := range builds {
		build := entry.Build
		state, when := snapshotBuildState(build, now)
		cells := []string{entry.Pool, shortSnapshotKey(build.Key), state, when, build.Box.Or("-"), snapshotBuildProgress(build.Progress), formatSnapshotPlan(build.Plan, true)}
		for i, cell := range cells {
			cells[i] = terminalSafe(cell)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// snapshotBuildState is a build's state and when it reached it.
func snapshotBuildState(build apimodel.PoolSnapshotBuild, now time.Time) (string, string) {
	if started, ok := build.StartedAt.Get(); ok {
		return "building", "started " + formatRelativeTime(now, started)
	}
	return "queued", "asked " + formatRelativeTime(now, build.RequestedAt)
}

// snapshotBuildProgress says what a build is provisioning: each feature
// under way, on its step, and how many of its features are done. A build
// that has reported nothing is "-".
func snapshotBuildProgress(reported apiclientgen.OptPoolSnapshotProgress) string {
	progress, ok := reported.Get()
	if !ok {
		return "-"
	}
	features := progress.Features.Or(nil)
	var running, failed []string
	done := 0
	for _, name := range slices.Sorted(maps.Keys(features)) {
		feature := features[name]
		switch feature.State {
		case "ready", "off":
			done++
		case "failed":
			done++
			failed = append(failed, name+" failed")
		case "provisioning":
			if step := feature.Step.Or(""); step != "" {
				running = append(running, name+": "+step)
			} else {
				running = append(running, name)
			}
		}
	}
	parts := append(running, failed...)
	if len(features) > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d done", done, len(features)))
	}
	if len(parts) == 0 {
		return progress.State
	}
	return truncateTableValue(strings.Join(parts, ", "), 80)
}
