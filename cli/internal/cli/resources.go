package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// newPoolResourcesCommand implements `discobox admin pool resources`: what a
// pool is consuming and which sandbox is consuming it.
//
// The ranking is the point. Every sandbox in a pool is differenced over the
// same tick by the one agent that polls all of them (ADR 0071), so the vCPU
// column is comparable between rows and adds up — which is what makes "who is
// eating this pool" a question the table answers rather than one it leaves to
// the reader.
func (a *App) newPoolResourcesCommand() *cobra.Command {
	var showProcesses bool
	cmd := &cobra.Command{
		Use:     "resources [POOL_ID]",
		Aliases: []string{"top"},
		Short:   "Show what a pool and its discoboxes are consuming",
		Long: `Show what a pool and its discoboxes are consuming.

CPU is reported in vCPU-equivalents: 1.00 is one core saturated, 3.70 is 3.7
cores' worth. The figure is a rate the pool agent computed by differencing
cumulative counters across one tick, so every discobox in the pool was measured
over the same window and the column both ranks and sums.

The overhead line is the load that is no discobox: the pool agent itself, the
shared BuildKit builder, the pool registry, and the proxy all run in the pool
container. A pool that looks busy with idle discoboxes usually has a build in
flight.

Memory is reported twice because both numbers are true. CHARGED is what the
host bills the discobox, including page cache; RESIDENT is what its processes
hold, summed, which double-counts shared pages and routinely exceeds CHARGED.

Disk is walked fresh on every report. The cache is shared by every discobox in
the pool, so it is counted once here and never per discobox.

Without POOL_ID the project's default pool is used.`,
		Example: `  discobox admin pool resources
  discobox admin pool resources pool_01hq
  discobox admin pool resources --processes`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completePools,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			var poolID string
			if len(args) == 1 {
				poolID, err = a.resolvePoolID(cmd.Context(), client, projectID, args[0])
			} else {
				poolID, err = a.defaultPoolID(cmd.Context(), client, projectID)
			}
			if err != nil {
				return err
			}
			if strings.TrimSpace(poolID) == "" {
				return fmt.Errorf("no pool given and this project has no default pool; pass a POOL_ID")
			}
			poolRes, err := client.GetPool(cmd.Context(), apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
			if err != nil {
				return err
			}
			pool, err := expectResponse[apimodel.Pool](poolRes)
			if err != nil {
				return err
			}
			// Every discobox in the project, filtered to this pool: the pool's
			// own report names them, but their consumption is recorded on their
			// own rows.
			listRes, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			list, err := expectResponse[apimodel.ListSandboxesBody](listRes)
			if err != nil {
				return err
			}
			return a.writePoolResources(cmd, pool, sandboxesInPool(list.GetSandboxes(), poolID), showProcesses)
		},
	}
	cmd.Flags().BoolVar(&showProcesses, "processes", false, "Show each discobox's busiest processes")
	return cmd
}

// newSandboxResourcesCommand implements `discobox admin discobox resources`:
// one discobox's consumption, down to the processes inside it.
func (a *App) newSandboxResourcesCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "resources DISCOBOX_ID",
		Aliases: []string{"top"},
		Short:   "Show what a discobox is consuming",
		Long: `Show what a discobox is consuming, down to the processes inside it.

Processes are ranked by rate rather than by total CPU used, so a test run that
started a minute ago outranks a language server that has been idling since
yesterday.

CPU is in vCPU-equivalents: 1.00 is one core saturated. VIRTUAL is address
space reserved rather than memory held, so a process that maps a large file
inflates it without consuming anything.

There is no cache figure here. The cache is one tree shared by every discobox
in the pool, so it is accounted once against the pool; see
` + "`discobox admin pool resources`" + `.`,
		Example:           `  discobox admin discobox resources sbx_01hq`,
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, sandboxID, client, err := a.sandboxRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sandboxRes, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandboxResources(cmd, sandbox)
		},
	}
}

// poolResourceReport decodes the pool's resource blob into the shape the
// contract documents for it.
//
// The blob is carried as an open object rather than a typed field, because it
// is written by whatever agent version the pool happens to be running: a
// strictly-validated field would fail the read of the whole pool during a
// rolling upgrade, over telemetry nothing schedules on. Decoding it here means
// a blob this build cannot read costs its own line, not the pool.
func poolResourceReport(pool *apimodel.Pool) (apimodel.PoolResourceReport, bool) {
	raw, ok := pool.Resources.Get()
	if !ok {
		return apimodel.PoolResourceReport{}, false
	}
	var report apimodel.PoolResourceReport
	if !decodeOpaque(raw, &report) {
		return apimodel.PoolResourceReport{}, false
	}
	return report, true
}

// sandboxResourceConsumption is the same for a sandbox's own blob.
func sandboxResourceConsumption(sandbox apimodel.Sandbox) (apimodel.SandboxResourceConsumption, bool) {
	raw, ok := sandbox.Runtime.Resources.Get()
	if !ok {
		return apimodel.SandboxResourceConsumption{}, false
	}
	var consumption apimodel.SandboxResourceConsumption
	if !decodeOpaque(raw, &consumption) {
		return apimodel.SandboxResourceConsumption{}, false
	}
	return consumption, true
}

// decodeOpaque reassembles an open object and decodes it through the generated
// unmarshaller, reporting false rather than an error: every caller's answer to
// a blob it cannot read is the same as its answer to no blob at all.
//
// The object is rebuilt with jx rather than encoding/json. A jx.Raw is a bare
// []byte with no MarshalJSON, so encoding/json base64-encodes each field
// instead of splicing it — which decodes to nothing, silently, and reads as a
// pool that never reported.
func decodeOpaque(raw map[string]jx.Raw, into json.Unmarshaler) bool {
	if len(raw) == 0 {
		return false
	}
	var encoder jx.Encoder
	encoder.ObjStart()
	for field, value := range raw {
		encoder.FieldStart(field)
		encoder.Raw(value)
	}
	encoder.ObjEnd()
	return into.UnmarshalJSON(encoder.Bytes()) == nil
}

func sandboxesInPool(sandboxes []apimodel.Sandbox, poolID string) []apimodel.Sandbox {
	out := make([]apimodel.Sandbox, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		if sandbox.PoolId.Or("") == poolID {
			out = append(out, sandbox)
		}
	}
	return out
}

func (a *App) writePoolResources(cmd *cobra.Command, pool *apimodel.Pool, sandboxes []apimodel.Sandbox, showProcesses bool) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), poolResourcesJSON(pool, sandboxes))
	}
	out := cmd.OutOrStdout()
	report, reported := poolResourceReport(pool)
	if !reported {
		// A pool that has not reported is not an idle pool. Saying so beats a
		// table of zeroes that reads like a measurement.
		fmt.Fprintf(out, "%s (%s) has not reported resource usage yet.\n", pool.ID, pool.Name)
		fmt.Fprintln(out, "A pool reports every 30s once its agent is up; a pool that is not ready never will.")
		return nil
	}

	fmt.Fprintf(out, "POOL %s  %s   reported %s\n\n", pool.ID, pool.Name, formatAge(report.ReportedAt))

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "CPU\t%s\n", formatPoolCPULine(report.CPU))
	fmt.Fprintf(tw, "MEMORY\t%s\n", formatPoolMemoryLine(report.Memory))
	fmt.Fprintf(tw, "DISK\t%s\n", formatFilesystemLine(report.Storage))
	fmt.Fprintf(tw, "\t%s\n", formatPoolTreesLine(report.Storage, sandboxes))
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(out)

	rows := rankSandboxRows(sandboxes)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "DISCOBOX\tNAME\tCPU\tCHARGED\tRESIDENT\tDISK\tPROC\tOBSERVED")
	for _, row := range rows {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			row.sandbox.ID,
			truncateTableValue(row.sandbox.DisplayName, 30),
			formatVCPUs(row.vcpus),
			formatOptBytes(row.chargedBytes),
			formatOptBytes(row.residentBytes),
			formatOptBytes(row.diskBytes),
			formatOptCount(row.processCount),
			row.observed,
		)
	}
	// The totals are the reason the rows are worth ranking. The pool's services
	// are a separate, disjoint measurement — the sandboxes run under a nested
	// runtime and are not in the pool container's cgroup — so the three lines
	// add rather than one being derived by subtracting from another.
	//
	// No separator row: a line with no tabs would end tabwriter's column block
	// and misalign the totals against the rows they total, and a row of empty
	// cells just draws trailing whitespace. The labels carry it instead.
	total := sumSandboxRows(rows)
	services := poolServicesRow(report)
	fmt.Fprintf(table, "discoboxes (%d)\t\t%s\t%s\t%s\t%s\t%s\t\n",
		len(rows),
		formatVCPUs(total.vcpus),
		formatOptBytes(total.chargedBytes),
		formatOptBytes(total.residentBytes),
		formatOptBytes(total.diskBytes),
		formatOptCount(total.processCount),
	)
	fmt.Fprintf(table, "pool services\t\t%s\t%s\t\t%s\t\t\n",
		formatVCPUs(services.vcpus),
		formatOptBytes(services.chargedBytes),
		formatOptBytes(services.diskBytes),
	)
	fmt.Fprintf(table, "total\t\t%s\t%s\t\t%s\t\t\n",
		formatVCPUs(total.vcpus.add(services.vcpus)),
		formatOptBytes(total.chargedBytes.add(services.chargedBytes)),
		formatOptBytes(total.diskBytes.add(services.diskBytes)),
	)
	if err := table.Flush(); err != nil {
		return err
	}

	if showProcesses {
		for _, row := range rows {
			consumption, ok := sandboxResourceConsumption(row.sandbox)
			if !ok || len(consumption.Processes) == 0 {
				continue
			}
			fmt.Fprintf(out, "\n%s  %s\n", row.sandbox.ID, truncateTableValue(row.sandbox.DisplayName, 40))
			if err := writeProcessTable(out, consumption.Processes); err != nil {
				return err
			}
		}
	}
	// The services line is a real measurement, not a remainder, but the
	// individual services in it are not broken out yet — so name what is in it
	// rather than leaving a number nobody can act on.
	fmt.Fprintln(out, "\nPool services are the pool container itself: the pool agent, the shared BuildKit")
	fmt.Fprintln(out, "builder, the pool registry, and the proxy. A build in flight lands there, not on")
	fmt.Fprintln(out, "any discobox.")
	return nil
}

func (a *App) writeSandboxResources(cmd *cobra.Command, sandbox *apimodel.Sandbox) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), sandboxResourcesJSON(sandbox))
	}
	out := cmd.OutOrStdout()
	consumption, ok := sandboxResourceConsumption(*sandbox)
	if !ok {
		fmt.Fprintf(out, "%s (%s) has no resource report yet.\n", sandbox.ID, sandbox.DisplayName)
		fmt.Fprintln(out, "Its pool reports every 30s; a discobox that has never run never reports one.")
		return nil
	}

	fmt.Fprintf(out, "DISCOBOX %s  %s   observed %s\n\n", sandbox.ID, sandbox.DisplayName, formatAge(consumption.ObservedAt.Or(time.Time{})))

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "CPU\t%s\n", formatSandboxCPULine(consumption))
	fmt.Fprintf(tw, "MEMORY\t%s\n", formatSandboxMemoryLine(consumption))
	fmt.Fprintf(tw, "DISK\t%s\n", formatSandboxDiskLine(consumption))
	if count, ok := consumption.ProcessCount.Get(); ok {
		fmt.Fprintf(tw, "PROCESSES\t%d\n", count)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	if len(consumption.Processes) == 0 {
		return nil
	}
	fmt.Fprintln(out)
	return writeProcessTable(out, consumption.Processes)
}

func writeProcessTable(out interface{ Write([]byte) (int, error) }, processes []apimodel.ProcessConsumption) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CPU\tRESIDENT\tVIRTUAL\tPID\tCOMMAND")
	for _, proc := range processes {
		rate := optFloat{}
		if vcpus, ok := proc.Vcpus.Get(); ok {
			rate = optFloat{value: vcpus, set: true}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\n",
			formatVCPUs(rate),
			formatBytes(proc.ResidentBytes),
			formatBytes(proc.VirtualBytes),
			proc.Pid,
			truncateTableValue(processCommand(proc), 70),
		)
	}
	return tw.Flush()
}

// processCommand prefers the full argv, which is what identifies a process a
// person recognizes: "node" names half a sandbox, "node .../vitest" names the
// thing actually eating the box.
func processCommand(proc apimodel.ProcessConsumption) string {
	if cmdline := strings.TrimSpace(proc.Cmdline.Or("")); cmdline != "" {
		return cmdline
	}
	return proc.Command
}

// sandboxResourceRow is one discobox's figures, flattened for ranking.
//
// Every field is optional because "not measured" and "zero" are different
// claims all the way through: a discobox whose agent has not reported has no
// rate, and printing 0.00 for it would say it is idle.
type sandboxResourceRow struct {
	sandbox       apimodel.Sandbox
	vcpus         optFloat
	chargedBytes  optInt64
	residentBytes optInt64
	diskBytes     optInt64
	processCount  optInt64
	observed      string
}

type optFloat struct {
	value float64
	set   bool
}

type optInt64 struct {
	value int64
	set   bool
}

func (o optInt64) add(other optInt64) optInt64 {
	if !other.set {
		return o
	}
	return optInt64{value: o.value + other.value, set: true}
}

func (o optFloat) add(other optFloat) optFloat {
	if !other.set {
		return o
	}
	return optFloat{value: o.value + other.value, set: true}
}

// rankSandboxRows orders by CPU rate, busiest first, then by resident size so
// a pool of idle discoboxes still ranks by what it is holding rather than
// arbitrarily. A discobox with no rate at all sorts last: not measured is not
// evidence of being busy.
func rankSandboxRows(sandboxes []apimodel.Sandbox) []sandboxResourceRow {
	rows := make([]sandboxResourceRow, 0, len(sandboxes))
	for _, sandbox := range sandboxes {
		rows = append(rows, newSandboxResourceRow(sandbox))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.vcpus.set != right.vcpus.set {
			return left.vcpus.set
		}
		if left.vcpus.value != right.vcpus.value {
			return left.vcpus.value > right.vcpus.value
		}
		return left.residentBytes.value > right.residentBytes.value
	})
	return rows
}

func newSandboxResourceRow(sandbox apimodel.Sandbox) sandboxResourceRow {
	row := sandboxResourceRow{sandbox: sandbox, observed: "-"}
	consumption, ok := sandboxResourceConsumption(sandbox)
	if !ok {
		return row
	}
	if observedAt, ok := consumption.ObservedAt.Get(); ok && !observedAt.IsZero() {
		row.observed = formatAge(observedAt)
	}
	if cpu, ok := consumption.CPU.Get(); ok {
		if vcpus, ok := cpu.Vcpus.Get(); ok {
			row.vcpus = optFloat{value: vcpus, set: true}
		}
	}
	if memory, ok := consumption.Memory.Get(); ok {
		row.chargedBytes = optInt64{value: memory.CurrentBytes, set: true}
		row.residentBytes = optInt64{value: memory.ResidentBytes, set: true}
	}
	if storage, ok := consumption.Storage.Get(); ok {
		row.diskBytes = optInt64{value: storage.TotalBytes, set: true}
	}
	if count, ok := consumption.ProcessCount.Get(); ok {
		row.processCount = optInt64{value: count, set: true}
	}
	return row
}

func sumSandboxRows(rows []sandboxResourceRow) sandboxResourceRow {
	var total sandboxResourceRow
	for _, row := range rows {
		total.vcpus = total.vcpus.add(row.vcpus)
		total.chargedBytes = total.chargedBytes.add(row.chargedBytes)
		total.residentBytes = total.residentBytes.add(row.residentBytes)
		total.diskBytes = total.diskBytes.add(row.diskBytes)
		total.processCount = total.processCount.add(row.processCount)
	}
	return total
}

func formatVCPUs(value optFloat) string {
	if !value.set {
		return "-"
	}
	return fmt.Sprintf("%.2f", value.value)
}

func formatOptBytes(value optInt64) string {
	if !value.set {
		return "-"
	}
	return formatBytes(value.value)
}

func formatOptCount(value optInt64) string {
	if !value.set {
		return "-"
	}
	return fmt.Sprintf("%d", value.value)
}

func formatPoolCPULine(cpu apimodel.PoolCPUUsage) string {
	vcpus, ok := cpu.Vcpus.Get()
	if !ok {
		// The first report after an agent restart has counters but no rate.
		return "not measured yet (needs two reports)"
	}
	capacity := cpu.CapacityVcpus.Or(0)
	if capacity <= 0 {
		return fmt.Sprintf("%.2f vCPU", vcpus)
	}
	return fmt.Sprintf("%.2f of %.2f vCPU  (%.0f%%)", vcpus, capacity, vcpus/capacity*100)
}

func formatPoolMemoryLine(memory apimodel.PoolMemoryUsage) string {
	line := fmt.Sprintf("%s charged", formatBytes(memory.CurrentBytes))
	if limit := memory.LimitBytes.Or(0); limit > 0 {
		line += fmt.Sprintf(" of %s limit", formatBytes(limit))
	}
	if available := memory.AvailableBytes.Or(0); available > 0 {
		line += fmt.Sprintf(", %s free on the host", formatBytes(available))
	}
	return line
}

// formatFilesystemLine reports the filesystem under everything, which usually
// holds more than Discobox — so it says whose used figure this is.
func formatFilesystemLine(storage apimodel.PoolStorageUsage) string {
	fs := storage.Filesystem
	if fs.TotalBytes <= 0 {
		return "not reported"
	}
	return fmt.Sprintf("%s used, %s free of %s on the filesystem under %s",
		formatBytes(fs.UsedBytes), formatBytes(fs.FreeBytes), formatBytes(fs.TotalBytes), storage.Root)
}

// formatPoolTreesLine is the walked attribution, which is deliberately older
// than the filesystem line above it. It says how old, and when it will be
// refreshed, because a reader who cannot tell a stale figure from a live one
// will read the stale one as live.
func formatPoolTreesLine(storage apimodel.PoolStorageUsage, sandboxes []apimodel.Sandbox) string {
	walk, ok := storage.Walk.Get()
	if !ok {
		return "per-tree usage not walked yet (the first sweep runs shortly after the agent starts)"
	}
	var sandboxBytes int64
	for _, sandbox := range sandboxes {
		if consumption, ok := sandboxResourceConsumption(sandbox); ok {
			if disk, ok := consumption.Storage.Get(); ok {
				sandboxBytes += disk.TotalBytes
			}
		}
	}
	return fmt.Sprintf("discoboxes %s, cache %s (shared), build %s   %s",
		formatBytes(sandboxBytes), formatBytes(walk.CacheBytes), formatBytes(walk.BuildBytes),
		formatWalkSchedule(walk))
}

// formatWalkSchedule explains the adaptive interval in the terms that make it
// legible: what the sweep cost, and what that bought. A pool being walked every
// fifty minutes looks like neglect until you can see that a sweep of it takes a
// minute.
func formatWalkSchedule(walk apimodel.PoolStorageWalk) string {
	line := fmt.Sprintf("[walked %s in %s", formatAge(walk.ObservedAt), formatScanDuration(walk.DurationMillis))
	if walk.IntervalSeconds > 0 {
		line += fmt.Sprintf(", every %s", formatInterval(time.Duration(walk.IntervalSeconds)*time.Second))
	}
	if !walk.NextScanAt.IsZero() {
		line += fmt.Sprintf(", next %s", formatDue(walk.NextScanAt))
	}
	return line + "]"
}

func formatScanDuration(millis int64) string {
	if millis < 1000 {
		return fmt.Sprintf("%dms", millis)
	}
	return (time.Duration(millis) * time.Millisecond).Round(100 * time.Millisecond).String()
}

func formatInterval(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Minute:
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}

// formatDue is the mirror of formatAge: how long until something is expected,
// or how overdue it already is. Overdue is worth naming — a sweep well past its
// due time means one failed or the agent stopped.
func formatDue(at time.Time) string {
	remaining := time.Until(at)
	if remaining <= 0 {
		return fmt.Sprintf("overdue by %s", formatInterval(-remaining))
	}
	return fmt.Sprintf("in %s", formatInterval(remaining))
}

func formatSandboxCPULine(consumption apimodel.SandboxResourceConsumption) string {
	cpu, ok := consumption.CPU.Get()
	if !ok {
		return "not reported"
	}
	line := "not measured yet (needs two reports)"
	if vcpus, ok := cpu.Vcpus.Get(); ok {
		line = fmt.Sprintf("%.2f vCPU", vcpus)
		if window := cpu.WindowSeconds.Or(0); window > 0 {
			line += fmt.Sprintf("  over %.0fs", window)
		}
	}
	if limit := cpu.LimitVcpus.Or(0); limit > 0 {
		line += fmt.Sprintf("  of a %.2f vCPU limit", limit)
	} else {
		// Worth saying plainly: nothing caps a discobox today, so one really
		// can starve its pool.
		line += "  (uncapped)"
	}
	if source := consumption.Source.Or(""); source != "" {
		line += fmt.Sprintf("  [%s]", source)
	}
	return line
}

func formatSandboxMemoryLine(consumption apimodel.SandboxResourceConsumption) string {
	memory, ok := consumption.Memory.Get()
	if !ok {
		return "not reported"
	}
	line := fmt.Sprintf("%s charged, %s resident, %s virtual",
		formatBytes(memory.CurrentBytes), formatBytes(memory.ResidentBytes), formatBytes(memory.VirtualBytes))
	if file := memory.FileBytes.Or(0); file > 0 {
		line += fmt.Sprintf("  (%s of the charge is reclaimable page cache)", formatBytes(file))
	}
	return line
}

func formatSandboxDiskLine(consumption apimodel.SandboxResourceConsumption) string {
	storage, ok := consumption.Storage.Get()
	if !ok {
		return "not reported"
	}
	return fmt.Sprintf("%s total   data %s, sources %s, config %s, secrets %s, origins %s",
		formatBytes(storage.TotalBytes),
		formatBytes(storage.DataBytes),
		formatBytes(storage.SourcesBytes),
		formatBytes(storage.ConfigBytes),
		formatBytes(storage.SecretsBytes),
		formatBytes(storage.OriginsBytes))
}

// poolServicesRow is what the pool runs that is not a discobox: the pool agent,
// the shared BuildKit builder, the pool registry, the proxy, and the mediator,
// all of which live in the pool container (ADR 0044).
//
// It is measured directly from that container's cgroup rather than derived by
// subtracting the discoboxes from a pool total. There is no such total to
// subtract from: the discoboxes run under a nested container runtime whose
// cgroups are not children of the pool container's, so the two measurements are
// disjoint and add (ADR 0071 §6). Deriving it by subtraction produced a
// negative on a live pool, because the pool figure was smaller than the sum of
// the discoboxes it was supposed to contain.
func poolServicesRow(report apimodel.PoolResourceReport) sandboxResourceRow {
	row := sandboxResourceRow{}
	if vcpus, ok := report.CPU.Vcpus.Get(); ok {
		row.vcpus = optFloat{value: vcpus, set: true}
	}
	row.chargedBytes = optInt64{value: report.Memory.CurrentBytes, set: true}
	// The pool's own disk is the shared cache and the build state, which no
	// discobox accounts for.
	if walk, ok := report.Storage.Walk.Get(); ok {
		row.diskBytes = optInt64{value: walk.CacheBytes + walk.BuildBytes, set: true}
	}
	return row
}

// formatAge is how long ago something was observed, which is what a reader of a
// 30s-interval report actually needs: an absolute timestamp makes them do the
// subtraction to find out whether they are looking at live data.
func formatAge(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	elapsed := time.Since(at)
	switch {
	case elapsed < 0:
		return "just now"
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
}

// poolResourcesJSON keeps the machine-readable form close to the wire shape:
// the pool's report as reported, and each discobox's as reported, rather than
// the flattened rows the table draws. A caller scripting against this wants the
// contract, not the layout.
func poolResourcesJSON(pool *apimodel.Pool, sandboxes []apimodel.Sandbox) map[string]any {
	out := map[string]any{"poolId": pool.ID, "name": pool.Name}
	// Held by pointer: these are ogen types whose optional fields encode
	// through their own MarshalJSON, and a value in an `any` is not
	// addressable, so encoding/json would reflect over the optionals instead
	// and fail on the unset ones.
	if report, ok := poolResourceReport(pool); ok {
		out["resources"] = &report
	}
	entries := make([]map[string]any, 0, len(sandboxes))
	for _, row := range rankSandboxRows(sandboxes) {
		entry := map[string]any{"sandboxId": row.sandbox.ID, "name": row.sandbox.DisplayName}
		if consumption, ok := sandboxResourceConsumption(row.sandbox); ok {
			entry["resources"] = &consumption
		}
		entries = append(entries, entry)
	}
	out["sandboxes"] = entries
	return out
}

func sandboxResourcesJSON(sandbox *apimodel.Sandbox) map[string]any {
	out := map[string]any{"sandboxId": sandbox.ID, "name": sandbox.DisplayName}
	if consumption, ok := sandboxResourceConsumption(*sandbox); ok {
		out["resources"] = &consumption
	}
	return out
}
