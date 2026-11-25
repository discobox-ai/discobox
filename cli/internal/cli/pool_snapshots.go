package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// A pool's snapshots, as its own resource report lists them (ADR
// 26-11-17-316 §7): what it holds, builds and failed to build. The pool alone
// decides what to do with any of them; this only shows them. A build's error
// can end with the build guest's own output, so every line is escaped as it
// is written (REVIEW.md).

// snapshotKeyWidth is how much of a snapshot's key a table shows: enough to
// tell a pool's snapshots apart and to find a box's in its pool's list.
const snapshotKeyWidth = 12

// poolSnapshotsSummary is the pool view's one-line account of its snapshots:
// how many it holds, builds and failed to build, or that its agent did not
// report them.
func poolSnapshotsSummary(pool apimodel.Pool) (string, *apimodel.PoolSnapshots) {
	report, ok := poolResourceReport(&pool)
	if !ok {
		return "not reported", nil
	}
	snapshots, ok := report.Snapshots.Get()
	if !ok {
		return "not reported", nil
	}
	if len(snapshots.Held)+len(snapshots.Building)+len(snapshots.Failed) == 0 {
		return "none", &snapshots
	}
	return fmt.Sprintf("%d held, %d building, %d failed", len(snapshots.Held), len(snapshots.Building), len(snapshots.Failed)), &snapshots
}

// writePoolSnapshots writes one row per snapshot, build and failed plan:
// its key, what it is doing, the plan it is for, when, and what else the
// pool said of it.
func writePoolSnapshots(w io.Writer, snapshots apimodel.PoolSnapshots, now time.Time) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SNAPSHOT\tSTATE\tWHEN\tPLAN\tDETAIL")
	row := func(cells ...string) {
		for i, cell := range cells {
			cells[i] = terminalSafe(cell)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	for _, held := range snapshots.Held {
		var detail []string
		if size, ok := held.SizeBytes.Get(); ok {
			detail = append(detail, formatBytes(size))
		}
		detail = append(detail, pluralCount(held.Boxes, "box", "boxes"))
		if used, ok := held.LastUsedAt.Get(); ok {
			detail = append(detail, "last used "+formatRelativeTime(now, used))
		}
		plan, _ := held.Plan.Get()
		row(shortSnapshotKey(held.Key), "held", "built "+formatRelativeTime(now, held.BuiltAt), formatSnapshotPlan(plan, held.Plan.Set), strings.Join(detail, ", "))
	}
	for _, build := range snapshots.Building {
		state, when := "queued", "asked "+formatRelativeTime(now, build.RequestedAt)
		if started, ok := build.StartedAt.Get(); ok {
			state, when = "building", "started "+formatRelativeTime(now, started)
		}
		row(shortSnapshotKey(build.Key), state, when, formatSnapshotPlan(build.Plan, true), "")
	}
	for _, failed := range snapshots.Failed {
		detail := "retry after " + formatRelativeTime(now, failed.RetryAfter)
		if reason, ok := failed.Error.Get(); ok {
			detail += ": " + truncateTableValue(reason, 160)
		}
		plan, _ := failed.Plan.Get()
		row(shortSnapshotKey(failed.Key), "failed", "failed "+formatRelativeTime(now, failed.FailedAt), formatSnapshotPlan(plan, failed.Plan.Set), detail)
	}
	return tw.Flush()
}

// formatSnapshotPlan names a plan by its bundles, the features it leaves
// off and the size a machine snapshot was built at, each bundle by its name and the start of its digest. The base and the
// agent are the same for every plan of a pool but across an upgrade, so they
// are left to the JSON output.
func formatSnapshotPlan(plan apimodel.PoolSnapshotPlan, known bool) string {
	if !known {
		return "(not recorded)"
	}
	parts := make([]string, 0, len(plan.Bundles)+1)
	for _, bundle := range plan.Bundles {
		parts = append(parts, bundle.Name+"@"+shortDigest(bundle.Digest))
	}
	if len(parts) == 0 {
		parts = append(parts, "base "+shortDigest(plan.Base))
	}
	if len(plan.Off) > 0 {
		parts = append(parts, "off: "+strings.Join(plan.Off, ","))
	}
	if size, ok := plan.Size.Get(); ok {
		var sized []string
		if cpus := size.Cpus.Or(0); cpus > 0 {
			sized = append(sized, fmt.Sprintf("%d vCPU", cpus))
		}
		if memory := size.MemoryBytes.Or(0); memory > 0 {
			sized = append(sized, formatBytes(memory))
		}
		if len(sized) > 0 {
			parts = append(parts, "size: "+strings.Join(sized, ","))
		}
	}
	return strings.Join(parts, " ")
}

// formatSandboxStartedFrom says what a box's container or machine was made
// from, by the snapshot's short key as its pool's view lists it.
func formatSandboxStartedFrom(sandbox apimodel.Sandbox) string {
	from, ok := sandbox.Runtime.StartedFrom.Get()
	switch {
	case !ok:
		return "-"
	case from.From == "snapshot":
		return "snapshot " + shortSnapshotKey(from.Snapshot.Or(""))
	default:
		return string(from.From)
	}
}

func shortSnapshotKey(key string) string {
	if len(key) > snapshotKeyWidth {
		return key[:snapshotKeyWidth]
	}
	return key
}

// shortDigest is a digest's hex, cut to a key's width.
func shortDigest(digest string) string {
	_, hex, ok := strings.Cut(digest, ":")
	if !ok {
		hex = digest
	}
	return shortSnapshotKey(hex)
}

func pluralCount(n int64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// startedFromPlan is the line naming the plan of the snapshot a box started
// from, when its pool lists that snapshot as held: "started from snapshot
// <key>: <plan>". Not ok when the box did not start from one, its pool cannot
// be read, or the pool does not list it (gone since, or not reported).
func (a *App) startedFromPlan(ctx context.Context, client *apiclientgen.Client, projectID string, sandbox apimodel.Sandbox) (string, bool) {
	from, ok := sandbox.Runtime.StartedFrom.Get()
	key := from.Snapshot.Or("")
	poolID := sandbox.PoolId.Or("")
	if !ok || from.From != "snapshot" || key == "" || poolID == "" {
		return "", false
	}
	res, err := client.GetPool(ctx, apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
	if err != nil {
		return "", false
	}
	pool, err := expectResponse[apimodel.Pool](res)
	if err != nil {
		return "", false
	}
	_, snapshots := poolSnapshotsSummary(*pool)
	if snapshots == nil {
		return "", false
	}
	for _, held := range snapshots.Held {
		if held.Key == key {
			plan, known := held.Plan.Get()
			return "started from snapshot " + key + ": " + formatSnapshotPlan(plan, known), true
		}
	}
	return "", false
}
