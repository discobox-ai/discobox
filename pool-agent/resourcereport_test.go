package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

func TestVCPURateIsCPUSecondsPerWallSecond(t *testing.T) {
	start := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	// 3.7 cores' worth over 10 seconds is 37 CPU-seconds.
	rate, ok := vcpuRate(1_000_000, 1_000_000+37_000_000, start, start.Add(10*time.Second))
	if !ok {
		t.Fatal("expected a rate")
	}
	if math.Abs(rate-3.7) > 1e-9 {
		t.Errorf("rate = %v, want 3.7", rate)
	}
}

// "No rate yet" and "idle" are different claims, and the report must not
// conflate them: a consumer ranking sandboxes by load would otherwise be told a
// sandbox that has only just been seen is doing nothing.
func TestVCPURateReportsNoRateRatherThanZero(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name               string
		prevUsec, currUsec int64
		prevAt, currAt     time.Time
	}{
		{"no previous sample", 0, 5_000_000, time.Time{}, now},
		{"same instant", 1_000, 2_000, now, now},
		{"clock went backwards", 1_000, 2_000, now, now.Add(-time.Second)},
		{"counter went backwards", 9_000_000, 1_000, now, now.Add(time.Second)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := vcpuRate(tc.prevUsec, tc.currUsec, tc.prevAt, tc.currAt); ok {
				t.Error("expected no rate")
			}
		})
	}
}

func sample(observedAt time.Time, usageUsec int64, procs ...apimodel.SandboxAgentProcessUsage) sandboxResourceSample {
	return sandboxResourceSample{Usage: apimodel.SandboxAgentResourceUsage{
		ObservedAt:   observedAt,
		Source:       apigen.SandboxAgentResourceUsageSourceCgroup,
		CPU:          apimodel.SandboxAgentCPUUsage{UsageUsec: usageUsec},
		Memory:       apimodel.SandboxAgentMemoryUsage{CurrentBytes: 1 << 30, VirtualBytes: 1 << 31, ResidentBytes: 1 << 30},
		ProcessCount: int64(len(procs)),
		Processes:    procs,
	}}
}

func proc(pid, startTicks, cpuUsec int64, command string, rss int64) apimodel.SandboxAgentProcessUsage {
	return apimodel.SandboxAgentProcessUsage{
		Pid: pid, StartTicks: startTicks, CpuUsec: cpuUsec,
		Command: command, ResidentBytes: rss,
	}
}

type stubResourceSource struct {
	samples map[string]sandboxResourceSample
}

func (s stubResourceSource) ResourceSamples() map[string]sandboxResourceSample { return s.samples }

func TestSandboxUsageDifferencesAgainstThePreviousReport(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	source := stubResourceSource{samples: map[string]sandboxResourceSample{
		"sbx_a": sample(base, 10_000_000),
	}}
	reporter := &poolResourceReporter{samples: source, previous: map[string]sandboxResourceSample{}}

	// First report: counters, but no rate to compute yet.
	first := reporter.sandboxUsage([]string{"sbx_a"}, nil)
	if len(first) != 1 || first[0].CPU == nil || first[0].CPU.UsageUsec != 10_000_000 {
		t.Fatalf("first report = %+v", first)
	}
	if first[0].CPU.VCPUs != nil {
		t.Errorf("first report carried a rate: %v", *first[0].CPU.VCPUs)
	}

	// Second report, 20s later, 30 CPU-seconds burned: 1.5 cores.
	source.samples["sbx_a"] = sample(base.Add(20*time.Second), 10_000_000+30_000_000)
	second := reporter.sandboxUsage([]string{"sbx_a"}, nil)
	if second[0].CPU.VCPUs == nil {
		t.Fatal("second report carried no rate")
	}
	if got := *second[0].CPU.VCPUs; math.Abs(got-1.5) > 1e-9 {
		t.Errorf("vcpus = %v, want 1.5", got)
	}
	if got := second[0].CPU.WindowSeconds; math.Abs(got-20) > 1e-9 {
		t.Errorf("windowSeconds = %v, want 20", got)
	}
}

// A sandbox that stops using CPU is idle, and must report 0.0 rather than
// nothing — the opposite of the not-yet-measured case above.
func TestSandboxUsageReportsIdleAsZero(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	source := stubResourceSource{samples: map[string]sandboxResourceSample{"sbx_a": sample(base, 5_000_000)}}
	reporter := &poolResourceReporter{samples: source, previous: map[string]sandboxResourceSample{}}
	reporter.sandboxUsage([]string{"sbx_a"}, nil)

	source.samples["sbx_a"] = sample(base.Add(30*time.Second), 5_000_000)
	got := reporter.sandboxUsage([]string{"sbx_a"}, nil)
	if got[0].CPU.VCPUs == nil {
		t.Fatal("an idle sandbox reported no rate; idle is a measurement, not an absence")
	}
	if *got[0].CPU.VCPUs != 0 {
		t.Errorf("vcpus = %v, want 0", *got[0].CPU.VCPUs)
	}
}

// A sandbox that reported nothing — stopped, or unreachable this tick — still
// holds its disk, and dropping it would make a full pool look empty.
func TestSandboxUsageKeepsStorageOnlySandboxes(t *testing.T) {
	reporter := &poolResourceReporter{
		samples:  stubResourceSource{samples: map[string]sandboxResourceSample{}},
		previous: map[string]sandboxResourceSample{},
	}
	storage := []SandboxStorage{{SandboxID: "sbx_stopped", DataBytes: 4096, TotalBytes: 4096}}

	got := reporter.sandboxUsage([]string{"sbx_stopped"}, storage)
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if got[0].Storage == nil || got[0].Storage.TotalBytes != 4096 {
		t.Errorf("storage = %+v, want the walked total", got[0].Storage)
	}
	// Absent, not zeroed: a zeroed currentBytes claims the sandbox holds no
	// memory, which is a different and false statement from "not measured".
	if got[0].CPU != nil || got[0].Memory != nil || got[0].ProcessCount != nil || got[0].ObservedAt != nil {
		t.Errorf("a sandbox that reported no counters carried measured fields: %+v", got[0])
	}
}

// A sandbox that vanishes must not leave a sample behind: if its ID were
// reissued, the new sandbox's first counter would be differenced against the
// dead one's.
func TestSandboxUsageForgetsSandboxesItNoLongerHosts(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	source := stubResourceSource{samples: map[string]sandboxResourceSample{"sbx_a": sample(base, 10_000_000)}}
	reporter := &poolResourceReporter{samples: source, previous: map[string]sandboxResourceSample{}}
	reporter.sandboxUsage([]string{"sbx_a"}, nil)

	reporter.sandboxUsage(nil, nil)
	if _, ok := reporter.previous["sbx_a"]; ok {
		t.Error("a sandbox no longer hosted was retained for differencing")
	}
}

// Ranking by the cumulative counter would put a long-lived idle daemon above a
// test run that has just started eating the box. Ranking by rate is the point.
func TestRankProcessesRanksByRateNotCumulative(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	// gopls has burned far more CPU in total but is doing nothing now; vitest
	// has just started and is burning 2 cores.
	previous := sample(base,
		0,
		proc(10, 100, 3_600_000_000, "gopls", 1<<30),
		proc(20, 900, 0, "vitest", 1<<28),
	).Usage
	current := []apimodel.SandboxAgentProcessUsage{
		proc(10, 100, 3_600_000_000, "gopls", 1<<30),
		proc(20, 900, 20_000_000, "vitest", 1<<28),
	}

	got := rankProcesses(current, base.Add(10*time.Second), previous)

	if len(got) == 0 {
		t.Fatal("no processes ranked")
	}
	if got[0].Command != "vitest" {
		t.Errorf("top process = %q, want vitest (the one actually burning CPU now)", got[0].Command)
	}
	if got[0].VCPUs == nil || math.Abs(*got[0].VCPUs-2.0) > 1e-9 {
		t.Errorf("vitest rate = %v, want 2.0", got[0].VCPUs)
	}
}

// PIDs are reused. Without the start time in the key, the recycled PID's small
// counter would be differenced against its predecessor's large one.
func TestRankProcessesDoesNotDifferenceARecycledPID(t *testing.T) {
	base := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	previous := sample(base, 0, proc(42, 100, 5_000_000_000, "old", 1<<20)).Usage
	// Same PID, different start time: a different process entirely.
	current := []apimodel.SandboxAgentProcessUsage{proc(42, 999_999, 1_000, "new", 1<<20)}

	got := rankProcesses(current, base.Add(10*time.Second), previous)

	if len(got) != 1 {
		t.Fatalf("got %d processes", len(got))
	}
	if got[0].VCPUs != nil {
		t.Errorf("a recycled PID was differenced against its predecessor: rate %v", *got[0].VCPUs)
	}
}

type stubRuntime struct {
	sandboxruntime.Runtime
	sandboxes []*sandboxruntime.Sandbox
	stored    []string
	err       error
	storedErr error
}

func (s stubRuntime) ListSandboxes(context.Context) ([]*sandboxruntime.Sandbox, error) {
	return s.sandboxes, s.err
}

func (s stubRuntime) StoredSandboxIDs(context.Context) ([]string, error) {
	return s.stored, s.storedErr
}

func testReporter(runtime sandboxruntime.Runtime) *poolResourceReporter {
	return &poolResourceReporter{runtime: runtime, logger: slog.New(slog.DiscardHandler)}
}

// Storage accounting must cover stopped sandboxes: they use no CPU but they
// still hold their trees.
func TestHostedSandboxIDsIncludesStoppedSandboxes(t *testing.T) {
	reporter := testReporter(stubRuntime{sandboxes: []*sandboxruntime.Sandbox{
		{SandboxID: "sbx_b", Status: sandboxruntime.StatusStopped},
		{SandboxID: "sbx_a", Status: sandboxruntime.StatusRunning},
		nil,
		{SandboxID: ""},
	}})

	ids, err := reporter.hostedSandboxIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "sbx_a" || ids[1] != "sbx_b" {
		t.Errorf("ids = %v, want [sbx_a sbx_b] sorted", ids)
	}
}

// An archived sandbox has no container at all — archiving keeps the tree and
// drops the container — and it is still occupying every byte it occupied while
// running. Enumerated from containers alone it would be invisible.
func TestHostedSandboxIDsIncludesArchivedTrees(t *testing.T) {
	reporter := testReporter(stubRuntime{
		sandboxes: []*sandboxruntime.Sandbox{{SandboxID: "sbx_live", Status: sandboxruntime.StatusRunning}},
		// sbx_live has both halves; sbx_archived is a tree and nothing else.
		stored: []string{"sbx_live", "sbx_archived", ""},
	})

	ids, err := reporter.hostedSandboxIDs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "sbx_archived" || ids[1] != "sbx_live" {
		t.Errorf("ids = %v, want [sbx_archived sbx_live] — the tree counts even with no container", ids)
	}
}

// A tree that cannot be read costs the trees, not the report: the containers
// are still worth reporting and the next tick tries again.
func TestHostedSandboxIDsSurvivesAnUnreadableTreeRoot(t *testing.T) {
	reporter := testReporter(stubRuntime{
		sandboxes: []*sandboxruntime.Sandbox{{SandboxID: "sbx_live", Status: sandboxruntime.StatusRunning}},
		storedErr: errors.New("permission denied"),
	})

	ids, err := reporter.hostedSandboxIDs(context.Background())
	if err != nil {
		t.Fatalf("an unreadable tree root failed the whole report: %v", err)
	}
	if len(ids) != 1 || ids[0] != "sbx_live" {
		t.Errorf("ids = %v, want the container that was readable", ids)
	}
}

func TestHostedSandboxIDsPropagatesRuntimeError(t *testing.T) {
	reporter := testReporter(stubRuntime{err: errors.New("boom")})
	if _, err := reporter.hostedSandboxIDs(context.Background()); err == nil {
		t.Error("expected the runtime error to propagate")
	}
}

func TestScanPoolStorageSeparatesEachSandboxTree(t *testing.T) {
	root := t.TempDir()
	// layout is rooted at an absolute container path, so the walk is exercised
	// through treeBytes directly against real directories here.
	write := func(dir string, size int) string {
		full := filepath.Join(root, dir)
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "f"), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
		return full
	}
	small := write("small", 100)
	large := write("large", 200_000)

	smallBytes := treeBytes(context.Background(), small)
	largeBytes := treeBytes(context.Background(), large)
	if smallBytes <= 0 || largeBytes <= 0 {
		t.Fatalf("treeBytes returned %d and %d", smallBytes, largeBytes)
	}
	if largeBytes <= smallBytes {
		t.Errorf("large tree (%d) did not exceed small tree (%d)", largeBytes, smallBytes)
	}
	// A tree that does not exist is zero, not an error: that is the truth for a
	// sandbox not yet created or already reaped.
	if got := treeBytes(context.Background(), filepath.Join(root, "absent")); got != 0 {
		t.Errorf("absent tree = %d, want 0", got)
	}
}

func TestWalkPoolTreesReportsEveryRequestedSandbox(t *testing.T) {
	// The trees are under the container root, which does not exist in a test,
	// so every figure is zero — but the shape must still name every sandbox
	// asked about rather than dropping the ones with nothing on disk.
	walk, ok := walkPoolTrees(context.Background(), "prj_1", "pool_1", []string{"sbx_a", "sbx_b"})
	if !ok {
		t.Fatal("sweep reported cancellation")
	}
	if len(walk.Sandboxes) != 2 {
		t.Fatalf("got %d sandboxes, want 2", len(walk.Sandboxes))
	}
	if walk.Sandboxes[0].SandboxID != "sbx_a" || walk.Sandboxes[1].SandboxID != "sbx_b" {
		t.Errorf("sandboxes = %+v", walk.Sandboxes)
	}
	if walk.ObservedAt.IsZero() {
		t.Error("a completed sweep must carry the moment it describes")
	}
}

func TestPoolFilesystemNamesTheContainerRoot(t *testing.T) {
	storage := poolFilesystem()
	if storage.Root != layout.ContainerRoot {
		t.Errorf("root = %q, want %q", storage.Root, layout.ContainerRoot)
	}
	// The walk is a separate, slower concern and is never filled in here.
	if storage.Walk != nil {
		t.Error("the cheap statfs path performed a walk")
	}
}

func TestFilesystemUsageFromBlocksExcludesReservedBlocks(t *testing.T) {
	// 1000 blocks total, 300 free, of which only 200 are available to an
	// unprivileged writer: 100 blocks are reserved and are neither used nor
	// available.
	usage := filesystemUsageFromBlocks(4096, 1000, 300, 200)

	if usage.TotalBytes != 1000*4096 {
		t.Errorf("total = %d", usage.TotalBytes)
	}
	if usage.UsedBytes != 700*4096 {
		t.Errorf("used = %d, want %d", usage.UsedBytes, 700*4096)
	}
	if usage.FreeBytes != 200*4096 {
		t.Errorf("free = %d, want %d", usage.FreeBytes, 200*4096)
	}
}

func TestFilesystemUsageFromBlocksSaturatesRatherThanOverflowing(t *testing.T) {
	usage := filesystemUsageFromBlocks(1<<20, math.MaxUint64, 0, 0)
	if usage.TotalBytes != math.MaxInt64 {
		t.Errorf("total = %d, want saturation at MaxInt64", usage.TotalBytes)
	}
}

func TestReadCgroupUsageReportsAbsenceRatherThanZeroes(t *testing.T) {
	if _, ok := readCgroupUsage(filepath.Join(t.TempDir(), "absent")); ok {
		t.Error("a missing cgroup hierarchy reported usage")
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "cpu.stat"),
		[]byte("usage_usec 500\nuser_usec 300\nsystem_usec 200\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.current"), []byte("2048\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	usage, ok := readCgroupUsage(root)
	if !ok {
		t.Fatal("expected usage")
	}
	if usage.CPUUsageUsec != 500 || usage.MemoryCurrentBytes != 2048 {
		t.Errorf("usage = %+v", usage)
	}
}

// The request this agent sends is hand-written, while the control plane decodes
// it into the type generated from server.yaml. Nothing but this test holds the
// two in agreement: a renamed or retyped field would otherwise be dropped in
// silence and show up as a resource report full of zeroes.
func TestPoolResourceReportRequestMatchesTheGeneratedContract(t *testing.T) {
	rate := 3.71
	poolRate := 8.1
	sandboxObservedAt := time.Date(2026, 8, 27, 11, 59, 57, 0, time.UTC)
	processCount := int64(42)
	sent := PoolResourceReportRequest{
		Report: PoolReport{
			ReportedAt: time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC),
			CPU: PoolCPUUsage{
				UsageUsec: 900_000_000, UserUsec: 700_000_000, SystemUsec: 200_000_000,
				VCPUs: &poolRate, WindowSeconds: 30, CapacityVCPUs: 16,
			},
			Memory: PoolMemoryUsage{CurrentBytes: 24 << 30, PeakBytes: 26 << 30, AvailableBytes: 8 << 30},
			Storage: PoolStorage{
				Root:       layout.ContainerRoot,
				Filesystem: FilesystemUsage{TotalBytes: 500 << 30, UsedBytes: 120 << 30, FreeBytes: 380 << 30},
				Walk: &PoolStorageWalk{
					ObservedAt:      time.Date(2026, 8, 27, 11, 52, 0, 0, time.UTC),
					DurationMillis:  11400,
					IntervalSeconds: 570,
					NextScanAt:      time.Date(2026, 8, 27, 12, 1, 30, 0, time.UTC),
					CacheBytes:      41 << 30,
					BuildBytes:      9 << 30,
					// Per-sandbox results ride the sandbox entries, never the
					// pool's storage record, so this must not reach the wire.
					Sandboxes: []SandboxStorage{{SandboxID: "sbx_a", DataBytes: 2048, TotalBytes: 6144}},
				},
			},
		},
		Sandboxes: []SandboxResourceUsage{{
			SandboxID:    "sbx_a",
			ObservedAt:   &sandboxObservedAt,
			Source:       "cgroup",
			CPU:          &SandboxCPUUsage{UsageUsec: 8_204_113_000, VCPUs: &rate, WindowSeconds: 30},
			Memory:       &SandboxMemoryUsage{CurrentBytes: 6 << 30, VirtualBytes: 12 << 30, ResidentBytes: 5 << 30},
			ProcessCount: &processCount,
			Processes:    []ProcessResourceUsage{{PID: 100, Command: "node", Cmdline: "node vitest", CPUUsec: 90, VCPUs: &rate, ResidentBytes: 1 << 30}},
			Storage:      &SandboxStorage{SandboxID: "sbx_a", DataBytes: 2048, TotalBytes: 6144},
		}},
	}

	body, err := json.Marshal(sent)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	var got apimodel.ReportPoolResourcesBody
	if err := got.UnmarshalJSON(body); err != nil {
		t.Fatalf("the control plane could not decode this agent's report: %v\n%s", err, body)
	}

	if !got.Report.ReportedAt.Equal(sent.Report.ReportedAt) {
		t.Errorf("reportedAt = %v, want %v", got.Report.ReportedAt, sent.Report.ReportedAt)
	}
	if got.Report.CPU.UsageUsec != sent.Report.CPU.UsageUsec || got.Report.CPU.Vcpus.Or(0) != poolRate {
		t.Errorf("pool cpu = %+v", got.Report.CPU)
	}
	if got.Report.CPU.CapacityVcpus.Or(0) != 16 {
		t.Errorf("capacityVcpus = %v, want 16", got.Report.CPU.CapacityVcpus.Or(0))
	}
	if got.Report.Memory.CurrentBytes != sent.Report.Memory.CurrentBytes || got.Report.Memory.AvailableBytes.Or(0) != 8<<30 {
		t.Errorf("pool memory = %+v", got.Report.Memory)
	}
	if got.Report.Storage.Root != layout.ContainerRoot {
		t.Errorf("pool storage root = %q", got.Report.Storage.Root)
	}
	if got.Report.Storage.Filesystem.FreeBytes != 380<<30 || got.Report.Storage.Filesystem.UsedBytes != 120<<30 {
		t.Errorf("filesystem = %+v", got.Report.Storage.Filesystem)
	}
	walk, ok := got.Report.Storage.Walk.Get()
	if !ok {
		t.Fatal("the walk did not survive the encoding")
	}
	if walk.CacheBytes != 41<<30 || walk.BuildBytes != 9<<30 {
		t.Errorf("walked totals = %+v", walk)
	}
	// The schedule is what makes a cached figure honest, so it has to cross.
	if walk.DurationMillis != 11400 || walk.IntervalSeconds != 570 {
		t.Errorf("walk schedule = %+v", walk)
	}
	if walk.ObservedAt.IsZero() || walk.NextScanAt.IsZero() {
		t.Errorf("walk timestamps = %+v", walk)
	}
	// Per-sandbox disk lives on the sandbox entries alone. Sending it on the
	// pool's walk record too would be the same figure in two places, free to
	// disagree the moment one is refreshed and the other is not.
	var raw struct {
		Report struct {
			Storage struct {
				Walk map[string]any `json:"walk"`
			} `json:"storage"`
		} `json:"report"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("re-decode request: %v", err)
	}
	if _, duplicated := raw.Report.Storage.Walk["sandboxes"]; duplicated {
		t.Error("per-sandbox storage was duplicated onto the pool's walk record")
	}

	if len(got.Sandboxes) != 1 {
		t.Fatalf("got %d sandboxes", len(got.Sandboxes))
	}
	sandbox := got.Sandboxes[0]
	if sandbox.SandboxId != "sbx_a" || sandbox.Source.Or("") != "cgroup" {
		t.Errorf("sandbox identity = %+v", sandbox)
	}
	cpu, ok := sandbox.CPU.Get()
	if !ok || cpu.UsageUsec != 8_204_113_000 || cpu.Vcpus.Or(0) != rate {
		t.Errorf("sandbox cpu = %+v (present=%v)", cpu, ok)
	}
	memory, ok := sandbox.Memory.Get()
	if !ok || memory.VirtualBytes != 12<<30 || memory.ResidentBytes != 5<<30 {
		t.Errorf("sandbox memory = %+v (present=%v)", memory, ok)
	}
	if sandbox.ProcessCount.Or(0) != 42 {
		t.Errorf("processCount = %v", sandbox.ProcessCount.Or(0))
	}
	if len(sandbox.Processes) != 1 || sandbox.Processes[0].Command != "node" {
		t.Errorf("processes = %+v", sandbox.Processes)
	}
	if storage, ok := sandbox.Storage.Get(); !ok || storage.TotalBytes != 6144 {
		t.Errorf("sandbox storage = %+v (present=%v)", storage, ok)
	}
}

// A stopped sandbox is not an unmeasured one. It is never polled, so it has no
// sample at all, and it contributes a known zero — conflating that with "no
// rate yet" meant a pool with one stopped sandbox never reported a total.
func TestTotalUsageCountsAStoppedSandboxAsZero(t *testing.T) {
	rate := 2.5
	running := SandboxResourceUsage{SandboxID: "sbx_a", CPU: &SandboxCPUUsage{VCPUs: &rate}}
	stopped := SandboxResourceUsage{SandboxID: "sbx_b"}
	services := 0.5
	report := PoolReport{
		CPU:    PoolCPUUsage{VCPUs: &services, CapacityVCPUs: 8},
		Memory: PoolMemoryUsage{CurrentBytes: 1 << 30, CapacityBytes: 16 << 30},
	}

	total := totalUsage(report, []SandboxResourceUsage{running, stopped})

	if total.VCPUs == nil {
		t.Fatal("a stopped sandbox suppressed the total")
	}
	if math.Abs(*total.VCPUs-3.0) > 1e-9 {
		t.Errorf("total = %v, want 3.0 (0.5 services + 2.5 running + 0 stopped)", *total.VCPUs)
	}
	if total.SandboxCount != 2 {
		t.Errorf("sandboxCount = %d, want 2", total.SandboxCount)
	}
}

// A sandbox that was sampled once and not yet twice genuinely has not been
// measured, and no total covering it is complete.
func TestTotalUsageIsAbsentWhileASandboxIsStillUnmeasured(t *testing.T) {
	services := 0.5
	unmeasured := SandboxResourceUsage{SandboxID: "sbx_a", CPU: &SandboxCPUUsage{UsageUsec: 1}}
	report := PoolReport{CPU: PoolCPUUsage{VCPUs: &services, CapacityVCPUs: 8}}

	if total := totalUsage(report, []SandboxResourceUsage{unmeasured}); total.VCPUs != nil {
		t.Errorf("total = %v, want absent while a sandbox is unmeasured", *total.VCPUs)
	}
}

// The services' own rate is part of the sum, so a pool that has not measured
// itself has no total either.
func TestTotalUsageIsAbsentWithoutAServicesRate(t *testing.T) {
	rate := 2.5
	report := PoolReport{CPU: PoolCPUUsage{CapacityVCPUs: 8}}

	total := totalUsage(report, []SandboxResourceUsage{{SandboxID: "sbx_a", CPU: &SandboxCPUUsage{VCPUs: &rate}}})
	if total.VCPUs != nil {
		t.Errorf("total = %v, want absent without a services rate", *total.VCPUs)
	}
	// Capacity is not a measurement and is always carried.
	if total.CapacityVCPUs != 8 {
		t.Errorf("capacity = %v, want 8", total.CapacityVCPUs)
	}
}
