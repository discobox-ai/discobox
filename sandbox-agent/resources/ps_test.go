package resources

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// psFixture is ps's output for psArgs in the shape macOS prints it: right-
// aligned numbers, CPU times as minutes:seconds.hundredths, rss and vsz in
// KiB, and the command line last with its own spacing.
const psFixture = `    1     0  12:01.50   4:00.25  20480 410000000 /sbin/launchd
  400     1   0:03.10   0:02.00  10240 400000000 /usr/local/bin/discobox-sandbox-agent serve
  500   400   1:00.00   0:45.00 204800 420000000 node  server.js --port 5173
  501   500   0:00.50   0:00.40   1024 400000100 /bin/sh -c sleep  60
  502   501   0:00.00   0:00.00    512 400000200 sleep 60
  600     1 1:02:03.45 1:00:00.00  4096 400000300 /usr/libexec/busyd
`

func TestParseCPUTime(t *testing.T) {
	for text, want := range map[string]int64{
		"0:00.00":    0,
		"0:00.03":    30_000,
		"12:34.56":   (12*60+34)*1_000_000 + 560_000,
		"1:02:03.45": (3600+2*60+3)*1_000_000 + 450_000,
		"2-01:02:03": (2*86400 + 3600 + 2*60 + 3) * 1_000_000,
		"0:01":       1_000_000,
	} {
		got, ok := parseCPUTime(text)
		if !ok || got != want {
			t.Errorf("parseCPUTime(%q) = %d, %v; want %d", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "12", "a:00.00", "1:-2.00", "1:2:3:4"} {
		if _, ok := parseCPUTime(text); ok {
			t.Errorf("parseCPUTime(%q) parsed, want refused", text)
		}
	}
}

func TestParsePSKeepsTheCommandLineAsPrinted(t *testing.T) {
	rows := parsePS(psFixture + "garbage line\n")
	if len(rows) != 6 {
		t.Fatalf("parsePS returned %d rows, want 6: %+v", len(rows), rows)
	}
	node := rows[2]
	if node.pid != 500 || node.ppid != 400 || node.args != "node  server.js --port 5173" {
		t.Fatalf("node row = %+v", node)
	}
	if node.cpuUsec != 60_000_000 || node.userUsec != 45_000_000 {
		t.Fatalf("node cpu = %d user = %d, want 60s and 45s", node.cpuUsec, node.userUsec)
	}
	if node.residentBytes != 204800*1024 || node.virtualBytes != 420000000*1024 {
		t.Fatalf("node memory = %d resident %d virtual, want KiB scaled to bytes", node.residentBytes, node.virtualBytes)
	}
}

func TestPSSampleIsTheProcessRollup(t *testing.T) {
	sampler := &psSampler{
		ps: func(context.Context) ([]byte, error) { return []byte(psFixture), nil },
		kernel: func() (map[int]kernelProc, error) {
			// 502 is missing: it started after the kernel table was read.
			return map[int]kernelProc{
				1:   {comm: "launchd", startTicks: 0},
				400: {comm: "discobox-sandbo", startTicks: 900},
				500: {comm: "node", startTicks: 12000},
				501: {comm: "sh", startTicks: 12100},
				600: {comm: "busyd", startTicks: 50},
			}, nil
		},
		memory: func() (int64, int64, error) { return 6 << 30, 16 << 30, nil },
	}
	usage := sampler.Sample(context.Background())

	if usage.Source != "proc" {
		t.Fatalf("source = %q, want proc: a darwin sandbox's totals are always the rollup", usage.Source)
	}
	if usage.ProcessCount != 6 {
		t.Fatalf("process count = %d, want every process ps listed", usage.ProcessCount)
	}
	wantCPU := int64((12*60+1)*1_000_000+500_000) + 3_100_000 + 60_000_000 + 500_000 + 0 + ((3600+2*60+3)*1_000_000 + 450_000)
	wantUser := int64(4*60*1_000_000+250_000) + 2_000_000 + 45_000_000 + 400_000 + 0 + 3600*1_000_000
	if usage.CPU.UsageUsec != wantCPU || usage.CPU.UserUsec != wantUser || usage.CPU.SystemUsec != wantCPU-wantUser {
		t.Fatalf("cpu = %+v, want usage %d user %d", usage.CPU, wantCPU, wantUser)
	}
	wantResident := int64(20480+10240+204800+1024+512+4096) * 1024
	if usage.Memory.ResidentBytes != wantResident {
		t.Fatalf("resident = %d, want the summed %d", usage.Memory.ResidentBytes, wantResident)
	}
	// Current and limit are the machine's, not the rollup's: the guest is
	// the sandbox, and summed resident size overcounts shared pages.
	if usage.Memory.CurrentBytes != 6<<30 || usage.Memory.LimitBytes != 16<<30 {
		t.Fatalf("memory = %+v, want the machine's 6 GiB in use of 16 GiB", usage.Memory)
	}
	if len(usage.Processes) != 5 {
		t.Fatalf("candidates = %+v, want the five processes with a start time", usage.Processes)
	}
	for _, proc := range usage.Processes {
		if proc.PID == 502 {
			t.Fatalf("candidate %+v has no start time to identify it", proc)
		}
		if proc.PID == 500 && (proc.Command != "node" || proc.StartTicks != 12000 || proc.Cmdline != "node  server.js --port 5173") {
			t.Fatalf("node candidate = %+v", proc)
		}
	}
}

func TestPSSampleReportsNothingWhenPSFails(t *testing.T) {
	sampler := &psSampler{
		ps:     func(context.Context) ([]byte, error) { return nil, errors.New("ps: exit status 1") },
		kernel: func() (map[int]kernelProc, error) { return nil, nil },
		memory: func() (int64, int64, error) { return 0, 0, errors.New("sysctl failed") },
	}
	usage := sampler.Sample(context.Background())
	if usage.Source != "proc" || usage.ProcessCount != 0 || usage.CPU.UsageUsec != 0 {
		t.Fatalf("usage = %+v, want an empty rollup", usage)
	}
}

func TestPSCollectReadsTheExecsProcessTree(t *testing.T) {
	sampler := &psSampler{
		ps:     func(context.Context) ([]byte, error) { return []byte(psFixture), nil },
		kernel: func() (map[int]kernelProc, error) { return nil, nil },
	}
	sample, err := sampler.Collect(context.Background(), execs.Exec{ID: "exec-1", PID: 500})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if sample.Source != "darwin-ps" || sample.TerminalID != "exec-1" {
		t.Fatalf("sample = %+v", sample)
	}
	var data struct {
		Terminal  map[string]any   `json:"terminal"`
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal(sample.Data, &data); err != nil {
		t.Fatal(err)
	}
	var pids []float64
	for _, proc := range data.Processes {
		pids = append(pids, proc["pid"].(float64))
	}
	if len(pids) != 3 || pids[0] != 500 || pids[1] != 501 || pids[2] != 502 {
		t.Fatalf("processes = %v, want 500 and what it started", pids)
	}
	if data.Terminal["id"] != "exec-1" {
		t.Fatalf("terminal = %+v", data.Terminal)
	}
}

func TestPSSampleFallsBackToResidentWithoutMachineMemory(t *testing.T) {
	sampler := &psSampler{
		ps:     func(context.Context) ([]byte, error) { return []byte(psFixture), nil },
		kernel: func() (map[int]kernelProc, error) { return nil, nil },
		memory: func() (int64, int64, error) { return 0, 0, errors.New("sysctl failed") },
	}
	usage := sampler.Sample(context.Background())
	if usage.Memory.CurrentBytes != usage.Memory.ResidentBytes || usage.Memory.LimitBytes != 0 {
		t.Fatalf("memory = %+v, want the rollup's resident size and no limit", usage.Memory)
	}
}

// TestPSSampleCPUNeverFallsWhenProcessesExit is what makes the darwin total a
// counter (ADR 0071): a process that exits keeps the CPU time it was last seen
// with, and a PID reused by a new process is a new process.
func TestPSSampleCPUNeverFallsWhenProcessesExit(t *testing.T) {
	outputs := []string{
		"  10     1   0:05.00   0:04.00  1024 1024 cc a.c\n" +
			"  11     1   0:02.00   0:01.00  1024 1024 cc b.c\n",
		// 10 exited; 11 ran on; 12 is new.
		"  11     1   0:03.00   0:02.00  1024 1024 cc b.c\n" +
			"  12     1   0:01.00   0:01.00  1024 1024 cc c.c\n",
		// 12's PID now belongs to a different process.
		"  11     1   0:03.00   0:02.00  1024 1024 cc b.c\n" +
			"  12     1   0:00.50   0:00.50  1024 1024 cc d.c\n",
	}
	starts := []map[int]kernelProc{
		{10: {comm: "cc", startTicks: 100}, 11: {comm: "cc", startTicks: 110}},
		{11: {comm: "cc", startTicks: 110}, 12: {comm: "cc", startTicks: 120}},
		{11: {comm: "cc", startTicks: 110}, 12: {comm: "cc", startTicks: 900}},
	}
	tick := 0
	sampler := &psSampler{
		ps:     func(context.Context) ([]byte, error) { return []byte(outputs[tick]), nil },
		kernel: func() (map[int]kernelProc, error) { return starts[tick], nil },
		memory: func() (int64, int64, error) { return 0, 0, errors.New("unused") },
	}
	want := []struct{ total, user int64 }{
		{7_000_000, 5_000_000},
		{5_000_000 + 3_000_000 + 1_000_000, 4_000_000 + 2_000_000 + 1_000_000},
		{5_000_000 + 1_000_000 + 3_000_000 + 500_000, 4_000_000 + 1_000_000 + 2_000_000 + 500_000},
	}
	for tick = range outputs {
		cpu := sampler.Sample(context.Background()).CPU
		if cpu.UsageUsec != want[tick].total || cpu.UserUsec != want[tick].user {
			t.Fatalf("sample %d cpu = %+v, want usage %d user %d", tick, cpu, want[tick].total, want[tick].user)
		}
		if cpu.SystemUsec != cpu.UsageUsec-cpu.UserUsec {
			t.Fatalf("sample %d system = %d, want usage minus user", tick, cpu.SystemUsec)
		}
	}
}

// TestPSSampleCPUHoldsThroughAFailedKernelRead covers a sample whose kernel
// table could not be read: its processes have no identity, and the ones
// already remembered must neither be retired — which would count their CPU
// twice on the next good sample — nor lost.
func TestPSSampleCPUHoldsThroughAFailedKernelRead(t *testing.T) {
	outputs := []string{
		"  10     1   0:05.00   0:04.00  1024 1024 cc a.c\n",
		// The kernel read fails; 10 ran on and 11 started.
		"  10     1   0:06.00   0:05.00  1024 1024 cc a.c\n" +
			"  11     1   0:01.00   0:01.00  1024 1024 cc b.c\n",
		"  10     1   0:07.00   0:06.00  1024 1024 cc a.c\n" +
			"  11     1   0:02.00   0:02.00  1024 1024 cc b.c\n",
		// 10 exited.
		"  11     1   0:03.00   0:03.00  1024 1024 cc b.c\n",
	}
	starts := []map[int]kernelProc{
		{10: {comm: "cc", startTicks: 100}},
		nil,
		{10: {comm: "cc", startTicks: 100}, 11: {comm: "cc", startTicks: 110}},
		{11: {comm: "cc", startTicks: 110}},
	}
	tick := 0
	sampler := &psSampler{
		ps: func(context.Context) ([]byte, error) { return []byte(outputs[tick]), nil },
		kernel: func() (map[int]kernelProc, error) {
			if starts[tick] == nil {
				return nil, errors.New("sysctl failed")
			}
			return starts[tick], nil
		},
		memory: func() (int64, int64, error) { return 0, 0, errors.New("unused") },
	}
	// 11 is left out of the failed sample: it has no identity yet, and is
	// counted from the sample that can name it.
	want := []int64{5_000_000, 6_000_000, 9_000_000, 7_000_000 + 3_000_000}
	for tick = range outputs {
		if got := sampler.Sample(context.Background()).CPU.UsageUsec; got != want[tick] {
			t.Fatalf("sample %d usage = %d, want %d", tick, got, want[tick])
		}
	}
}

// TestPSSampleCPUNeverFallsForAProcessTheKernelDidNotName covers the processes
// a sample cannot identify: one ps listed that exited before the kernel table
// was read, and a tracked PID taken by another process while the table could
// not be read. Neither may pull the total down.
func TestPSSampleCPUNeverFallsForAProcessTheKernelDidNotName(t *testing.T) {
	outputs := []string{
		"  10     1   0:05.00   0:04.00  1024 1024 cc a.c\n",
		// 20 is a compiler that exits before the kernel table is read.
		"  10     1   0:06.00   0:05.00  1024 1024 cc a.c\n" +
			"  20     1   0:02.00   0:02.00  1024 1024 cc z.c\n",
		// The kernel read fails, and 10's PID now belongs to a new process.
		"  10     1   0:00.50   0:00.50  1024 1024 cc b.c\n",
		"  10     1   0:01.00   0:01.00  1024 1024 cc b.c\n",
	}
	starts := []map[int]kernelProc{
		{10: {comm: "cc", startTicks: 100}},
		{10: {comm: "cc", startTicks: 100}},
		nil,
		{10: {comm: "cc", startTicks: 900}},
	}
	tick := 0
	sampler := &psSampler{
		ps: func(context.Context) ([]byte, error) { return []byte(outputs[tick]), nil },
		kernel: func() (map[int]kernelProc, error) {
			if starts[tick] == nil {
				return nil, errors.New("sysctl failed")
			}
			return starts[tick], nil
		},
		memory: func() (int64, int64, error) { return 0, 0, errors.New("unused") },
	}
	want := []int64{
		5_000_000,
		6_000_000,
		// The old process's 6s is held rather than replaced by 0.5s.
		6_000_000,
		// The old process retires with its 6s; the new one counts its own.
		6_000_000 + 1_000_000,
	}
	var last int64
	for tick = range outputs {
		got := sampler.Sample(context.Background()).CPU.UsageUsec
		if got != want[tick] {
			t.Fatalf("sample %d usage = %d, want %d", tick, got, want[tick])
		}
		if got < last {
			t.Fatalf("sample %d usage fell from %d to %d", tick, last, got)
		}
		last = got
	}
}
