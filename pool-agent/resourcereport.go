package poolagent

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"sort"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

const (
	// resourceReportInterval paces the resource report. It is deliberately
	// slower than the status poll it reads samples from: this loop walks every
	// tree the pool owns on every report, with nothing cached, and that walk is
	// what the wider interval pays for (ADR 0071 §7).
	resourceReportInterval = 30 * time.Second
	// resourceReportTimeout bounds one report, for the same reason the status
	// heartbeat is bounded: an unbounded post against a wedged control plane
	// would stall every later report.
	resourceReportTimeout = 30 * time.Second
	// reportedProcesses is how many processes one sandbox names in a report,
	// per ranking. The sandbox itself offers a wider candidate list; this is
	// what survives being ranked by rate.
	reportedProcesses = 5
)

// PoolResourceClient pushes this pool's resource accounting to the control
// plane.
type PoolResourceClient interface {
	ReportPoolResources(ctx context.Context, req PoolResourceReportRequest) error
}

// PoolResourceReportRequest is one resource report: what this pool is using,
// and what each sandbox on it is using, in one delivery (ADR 0071).
//
// Every rate here was computed by this agent, from cumulative counters, over
// one tick. That is what makes the per-sandbox figures rankable: one component
// polls every sandbox in the pool, so all of them are measured over the same
// window and the ranking compares like with like. Rates computed independently
// inside each sandbox would each cover a slightly different window.
type PoolResourceReportRequest struct {
	ControlPlaneURL string             `json:"-"`
	ProjectID       string             `json:"-"`
	PoolID          string             `json:"-"`
	PrivateKey      ed25519.PrivateKey `json:"-"`

	// Report is the pool-wide half, which the control plane stores verbatim on
	// the pool row. It is nested rather than inlined so that the shape stored
	// and the shape reported are the same object and cannot drift apart.
	Report    PoolReport             `json:"report"`
	Sandboxes []SandboxResourceUsage `json:"sandboxes"`
}

// PoolReport is what this pool's own services are using.
//
// CPU and Memory come from the pool container's own cgroup: this agent,
// buildkitd, the registry, the proxy, and the mediator. They deliberately
// exclude the sandboxes, which run under a nested container runtime whose
// cgroups are not children of this one — so the pool's total load is this plus
// the per-sandbox figures, an addition of two disjoint measurements.
//
// It was originally the other way round: the pool figure was taken for a total
// and the services derived by subtracting the sandboxes from it. On a live pool
// that produced a pool total *smaller* than its own sandboxes' sum, because the
// two sets never overlapped (ADR 0071 §6).
type PoolReport struct {
	ReportedAt time.Time       `json:"reportedAt"`
	CPU        PoolCPUUsage    `json:"cpu"`
	Memory     PoolMemoryUsage `json:"memory"`
	Storage    PoolStorage     `json:"storage"`
}

// PoolCPUUsage is the pool services' cumulative CPU counter and the rate
// derived from it.
type PoolCPUUsage struct {
	UsageUsec  int64 `json:"usageUsec"`
	UserUsec   int64 `json:"userUsec"`
	SystemUsec int64 `json:"systemUsec"`
	// VCPUs is nil when there was no previous sample to difference — the first
	// report after this agent started, above all. That is deliberately not
	// zero: "not measured yet" and "idle" are different claims, and a consumer
	// ranking pools by load must not be told a starting pool is idle.
	VCPUs         *float64 `json:"vcpus,omitempty"`
	WindowSeconds float64  `json:"windowSeconds,omitempty"`
	// CapacityVCPUs is how many CPUs the host has, so a rate has something to
	// be read against.
	CapacityVCPUs float64 `json:"capacityVcpus,omitempty"`
}

// PoolMemoryUsage is what the pool's own services hold, as the pool container's
// cgroup charges them.
type PoolMemoryUsage struct {
	CurrentBytes int64 `json:"currentBytes"`
	PeakBytes    int64 `json:"peakBytes,omitempty"`
	LimitBytes   int64 `json:"limitBytes,omitempty"`
	// AvailableBytes is what the host still has free, which is what the pool
	// is actually bounded by when the cgroup carries no limit.
	AvailableBytes int64 `json:"availableBytes,omitempty"`
	// CapacityBytes is how much memory the host has, so a figure has something
	// to be read against — the counterpart of PoolCPUUsage.CapacityVCPUs.
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
}

// SandboxResourceUsage is one sandbox's consumption, as this agent computed it
// from the counters that sandbox reported.
// Every measured field is optional, and omitted rather than zeroed when this
// sandbox reported no counters — stopped, unreachable this tick, or running an
// agent that does not report them. A zeroed `currentBytes` is a claim that the
// sandbox holds no memory, which is a different and false statement from "not
// measured". Storage is separate and usually still present: a stopped sandbox
// uses no CPU but still holds its disk.
type SandboxResourceUsage struct {
	SandboxID  string     `json:"sandboxId"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	// Source is what the sandbox said its own totals came from: "cgroup" is
	// authoritative, "proc" means it could not read its cgroup and a
	// per-process rollup stood in, which sees no page cache.
	Source       string                 `json:"source,omitempty"`
	CPU          *SandboxCPUUsage       `json:"cpu,omitempty"`
	Memory       *SandboxMemoryUsage    `json:"memory,omitempty"`
	ProcessCount *int64                 `json:"processCount,omitempty"`
	Processes    []ProcessResourceUsage `json:"processes,omitempty"`
	Storage      *SandboxStorage        `json:"storage,omitempty"`
}

// SandboxCPUUsage carries both the counter and the rate: the counter so a
// consumer can difference across any two reports it kept, the rate so it does
// not have to.
type SandboxCPUUsage struct {
	UsageUsec     int64    `json:"usageUsec"`
	UserUsec      int64    `json:"userUsec"`
	SystemUsec    int64    `json:"systemUsec"`
	VCPUs         *float64 `json:"vcpus,omitempty"`
	WindowSeconds float64  `json:"windowSeconds,omitempty"`
	LimitVCPUs    float64  `json:"limitVcpus,omitempty"`
}

// SandboxMemoryUsage reports what the sandbox costs the pool and what its
// processes hold, which are different numbers and both true (ADR 0071 §4).
type SandboxMemoryUsage struct {
	CurrentBytes  int64 `json:"currentBytes"`
	PeakBytes     int64 `json:"peakBytes,omitempty"`
	AnonBytes     int64 `json:"anonBytes,omitempty"`
	FileBytes     int64 `json:"fileBytes,omitempty"`
	LimitBytes    int64 `json:"limitBytes,omitempty"`
	VirtualBytes  int64 `json:"virtualBytes"`
	ResidentBytes int64 `json:"residentBytes"`
}

// ProcessResourceUsage is one process inside a sandbox, ranked by what it is
// using now rather than by what it has used since it started.
type ProcessResourceUsage struct {
	PID           int64    `json:"pid"`
	Command       string   `json:"command"`
	Cmdline       string   `json:"cmdline,omitempty"`
	CPUUsec       int64    `json:"cpuUsec"`
	VCPUs         *float64 `json:"vcpus,omitempty"`
	VirtualBytes  int64    `json:"virtualBytes"`
	ResidentBytes int64    `json:"residentBytes"`
}

// startPoolResourceReporter runs the standing loop that reports what this pool
// and its sandboxes are using.
//
// It never affects sandbox lifecycle. A failed scan, a sandbox that reported no
// counters, or a failed push is logged and skipped for this report; the next
// tick is the retry, exactly as it is for the status poll this loop reads from.
func startPoolResourceReporter(
	ctx context.Context,
	logger *slog.Logger,
	bootstrap Bootstrap,
	registration *Registration,
	runtime sandboxruntime.Runtime,
	samples sandboxResourceSource,
	client PoolResourceClient,
) {
	if runtime == nil || client == nil || registration == nil || samples == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	reporter := &poolResourceReporter{
		logger:       logger,
		bootstrap:    bootstrap,
		registration: registration,
		runtime:      runtime,
		samples:      samples,
		client:       client,
		previous:     map[string]sandboxResourceSample{},
	}
	// The disk walk runs on its own adaptive schedule rather than this tick.
	// Reading CPU and memory is a handful of small files; walking disk is one
	// pass over every inode the pool owns, and a schedule that suits one suits
	// the other only by accident (ADR 0071 §7).
	reporter.storage = newStorageScanner(logger, bootstrap, func(ctx context.Context) []string {
		ids, err := reporter.hostedSandboxIDs(ctx)
		if err != nil {
			logger.Warn("list sandboxes for storage scan", "error", err)
			return nil
		}
		return ids
	})
	reporter.storage.start(ctx)
	go func() {
		ticker := time.NewTicker(resourceReportInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				reporter.report(ctx)
			}
		}
	}()
}

// sandboxResourceSource hands over the most recent counters the status poller
// collected. The two loops share one poll of each sandbox rather than each
// making their own: the counters are a by-product of a call the status poller
// already makes every 15 seconds, and polling a sandbox twice to read the same
// two numbers would be waste, not independence.
type sandboxResourceSource interface {
	ResourceSamples() map[string]sandboxResourceSample
}

// sandboxResourceSample is one sandbox's counters as of its last status poll,
// decoded out of the payload the poller relays untouched.
type sandboxResourceSample struct {
	Usage apimodel.SandboxAgentResourceUsage
}

type poolResourceReporter struct {
	logger       *slog.Logger
	bootstrap    Bootstrap
	registration *Registration
	runtime      sandboxruntime.Runtime
	samples      sandboxResourceSource
	client       PoolResourceClient
	storage      *storageScanner

	// previous is the last sample this loop differenced against, per sandbox,
	// plus the pool's own under poolCgroupKey. It is this loop's own memory and
	// deliberately not the poller's: the poller holds the newest sample, while
	// what a rate needs is the one from the previous report.
	previous     map[string]sandboxResourceSample
	previousPool cgroupUsage
	previousAt   time.Time
}

func (r *poolResourceReporter) report(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, resourceReportTimeout)
	defer cancel()

	sandboxIDs, err := r.hostedSandboxIDs(ctx)
	if err != nil {
		r.logger.Warn("list sandboxes for resource report", "error", err)
		return
	}
	now := time.Now().UTC()
	// The filesystem is stat'd on every report because it costs one syscall and
	// is the figure that answers "am I about to run out of disk". The walked
	// attribution is whatever the scanner last completed, carrying its own
	// timestamps so a reader can see how old it is.
	storage := poolFilesystem()
	storage.Walk = r.storage.Snapshot()
	request := PoolResourceReportRequest{
		ControlPlaneURL: r.bootstrap.ControlPlaneURL,
		ProjectID:       r.bootstrap.ProjectID,
		PoolID:          r.bootstrap.PoolID,
		PrivateKey:      r.registration.PrivateKey,
		Report:          PoolReport{ReportedAt: now, Storage: storage},
	}
	request.Report.CPU, request.Report.Memory = r.poolUsage(now)
	request.Sandboxes = r.sandboxUsage(sandboxIDs, walkedSandboxStorage(storage.Walk))

	if err := r.client.ReportPoolResources(ctx, request); err != nil {
		r.logger.Warn("push pool resource report", "poolId", r.bootstrap.PoolID, "error", err)
	}
}

// hostedSandboxIDs is every sandbox this pool hosts, running or not. Unlike the
// status poll, a stopped sandbox is not skipped: it uses no CPU but it still
// holds its disk, and a sandbox that vanished from the accounting the moment it
// stopped would make a full pool look empty.
func (r *poolResourceReporter) hostedSandboxIDs(ctx context.Context) ([]string, error) {
	sandboxes, err := r.runtime.ListSandboxes(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(sandboxes))
	for _, sb := range sandboxes {
		if sb != nil && sb.SandboxID != "" {
			ids = append(ids, sb.SandboxID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// walkedSandboxStorage is the last sweep's per-sandbox results, or nothing when
// no sweep has finished. Each entry is attached to its own sandbox below rather
// than traveling on the pool's storage record too: one figure, one place.
func walkedSandboxStorage(walk *PoolStorageWalk) []SandboxStorage {
	if walk == nil {
		return nil
	}
	return walk.Sandboxes
}

func (r *poolResourceReporter) poolUsage(now time.Time) (PoolCPUUsage, PoolMemoryUsage) {
	cpu := PoolCPUUsage{CapacityVCPUs: availableCPUVCPUs()}
	memory := PoolMemoryUsage{AvailableBytes: availableMemoryBytes(), CapacityBytes: totalMemoryBytes()}
	usage, ok := readCgroupUsage(poolCgroupRoot)
	if !ok {
		return cpu, memory
	}
	cpu.UsageUsec = usage.CPUUsageUsec
	cpu.UserUsec = usage.CPUUserUsec
	cpu.SystemUsec = usage.CPUSystemUsec
	if rate, ok := vcpuRate(r.previousPool.CPUUsageUsec, usage.CPUUsageUsec, r.previousAt, now); ok {
		cpu.VCPUs = &rate
		cpu.WindowSeconds = windowSeconds(r.previousAt, now)
	}
	memory.CurrentBytes = usage.MemoryCurrentBytes
	memory.PeakBytes = usage.MemoryPeakBytes
	memory.LimitBytes = usage.MemoryLimitBytes

	r.previousPool = usage
	r.previousAt = now
	return cpu, memory
}

// sandboxUsage differences this tick's counters against the last report's, for
// every sandbox that reported any, and attaches the storage already walked for
// it.
//
// A sandbox with counters but no previous sample is still reported: the
// counters and its disk are real, and only the rate is missing. A sandbox that
// reported no counters at all — stopped, unreachable, or on a platform without
// this accounting — is reported with its storage alone.
func (r *poolResourceReporter) sandboxUsage(sandboxIDs []string, storage []SandboxStorage) []SandboxResourceUsage {
	current := r.samples.ResourceSamples()
	byID := make(map[string]*SandboxStorage, len(storage))
	for i := range storage {
		byID[storage[i].SandboxID] = &storage[i]
	}

	out := make([]SandboxResourceUsage, 0, len(sandboxIDs))
	for _, sandboxID := range sandboxIDs {
		entry := SandboxResourceUsage{SandboxID: sandboxID, Storage: byID[sandboxID]}
		if sample, ok := current[sandboxID]; ok {
			r.applySample(&entry, sample, r.previous[sandboxID])
		}
		out = append(out, entry)
	}
	// Keep only what this tick saw, so a reaped sandbox's sample cannot sit in
	// memory forever and cannot be differenced against if its ID is reissued.
	next := make(map[string]sandboxResourceSample, len(current))
	for _, sandboxID := range sandboxIDs {
		if sample, ok := current[sandboxID]; ok {
			next[sandboxID] = sample
		}
	}
	r.previous = next
	return out
}

func (r *poolResourceReporter) applySample(entry *SandboxResourceUsage, sample, previous sandboxResourceSample) {
	usage := sample.Usage
	observedAt := usage.ObservedAt
	entry.ObservedAt = &observedAt
	entry.Source = string(usage.Source)
	processCount := usage.ProcessCount
	entry.ProcessCount = &processCount
	entry.CPU = &SandboxCPUUsage{
		UsageUsec:  usage.CPU.UsageUsec,
		UserUsec:   usage.CPU.UserUsec,
		SystemUsec: usage.CPU.SystemUsec,
		LimitVCPUs: usage.CPU.LimitVcpus.Or(0),
	}
	if rate, ok := vcpuRate(previous.Usage.CPU.UsageUsec, usage.CPU.UsageUsec, previous.Usage.ObservedAt, usage.ObservedAt); ok {
		entry.CPU.VCPUs = &rate
		entry.CPU.WindowSeconds = windowSeconds(previous.Usage.ObservedAt, usage.ObservedAt)
	}
	entry.Memory = &SandboxMemoryUsage{
		CurrentBytes:  usage.Memory.CurrentBytes,
		PeakBytes:     usage.Memory.PeakBytes.Or(0),
		AnonBytes:     usage.Memory.AnonBytes.Or(0),
		FileBytes:     usage.Memory.FileBytes.Or(0),
		LimitBytes:    usage.Memory.LimitBytes.Or(0),
		VirtualBytes:  usage.Memory.VirtualBytes,
		ResidentBytes: usage.Memory.ResidentBytes,
	}
	entry.Processes = rankProcesses(usage.Processes, usage.ObservedAt, previous.Usage)
}

// rankProcesses turns the sandbox's candidate list into the answer to "what is
// using this", by differencing each process against itself in the previous
// sample and keeping the busiest by rate and the largest by resident size.
//
// Ranking by rate rather than by the cumulative counter is the point. A
// language server that has been up for a day outranks a test run that started
// two minutes ago on cumulative CPU, and is not what anyone means by "what is
// using all the CPU".
func rankProcesses(candidates []apimodel.SandboxAgentProcessUsage, observedAt time.Time, previous apimodel.SandboxAgentResourceUsage) []ProcessResourceUsage {
	if len(candidates) == 0 {
		return nil
	}
	before := make(map[processKey]apimodel.SandboxAgentProcessUsage, len(previous.Processes))
	for _, proc := range previous.Processes {
		before[processKey{pid: proc.Pid, startTicks: proc.StartTicks}] = proc
	}

	all := make([]ProcessResourceUsage, 0, len(candidates))
	for _, proc := range candidates {
		entry := ProcessResourceUsage{
			PID:           proc.Pid,
			Command:       proc.Command,
			Cmdline:       proc.Cmdline.Or(""),
			CPUUsec:       proc.CpuUsec,
			VirtualBytes:  proc.VirtualBytes,
			ResidentBytes: proc.ResidentBytes,
		}
		if prior, ok := before[processKey{pid: proc.Pid, startTicks: proc.StartTicks}]; ok {
			if rate, ok := vcpuRate(prior.CpuUsec, proc.CpuUsec, previous.ObservedAt, observedAt); ok {
				entry.VCPUs = &rate
			}
		}
		all = append(all, entry)
	}
	return topProcesses(all)
}

// topProcesses is the union of the busiest by rate and the largest by resident
// size, deduplicated by PID and capped. A process with no rate yet sorts last
// among the busy, since "not measured" is not evidence of being busy.
func topProcesses(all []ProcessResourceUsage) []ProcessResourceUsage {
	byCPU := make([]ProcessResourceUsage, len(all))
	copy(byCPU, all)
	sort.SliceStable(byCPU, func(i, j int) bool {
		return rateOrZero(byCPU[i].VCPUs) > rateOrZero(byCPU[j].VCPUs)
	})
	byRSS := make([]ProcessResourceUsage, len(all))
	copy(byRSS, all)
	sort.SliceStable(byRSS, func(i, j int) bool {
		return byRSS[i].ResidentBytes > byRSS[j].ResidentBytes
	})

	seen := make(map[int64]bool, reportedProcesses*2)
	out := make([]ProcessResourceUsage, 0, reportedProcesses*2)
	for _, ranked := range [][]ProcessResourceUsage{byCPU, byRSS} {
		for i := 0; i < reportedProcesses && i < len(ranked); i++ {
			if seen[ranked[i].PID] {
				continue
			}
			seen[ranked[i].PID] = true
			out = append(out, ranked[i])
		}
	}
	return out
}

func rateOrZero(rate *float64) float64 {
	if rate == nil {
		return 0
	}
	return *rate
}
