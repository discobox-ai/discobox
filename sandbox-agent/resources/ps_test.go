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
	sampler := psSampler{
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
	if usage.Memory.ResidentBytes != wantResident || usage.Memory.CurrentBytes != wantResident {
		t.Fatalf("memory = %+v, want resident and current %d", usage.Memory, wantResident)
	}
	if usage.Memory.LimitBytes != 0 || usage.CPU.LimitVCPUs != 0 {
		t.Fatalf("limits = %d bytes %v vcpus, want none: there is no cgroup quota", usage.Memory.LimitBytes, usage.CPU.LimitVCPUs)
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
	sampler := psSampler{
		ps:     func(context.Context) ([]byte, error) { return nil, errors.New("ps: exit status 1") },
		kernel: func() (map[int]kernelProc, error) { return nil, nil },
	}
	usage := sampler.Sample(context.Background())
	if usage.Source != "proc" || usage.ProcessCount != 0 || usage.CPU.UsageUsec != 0 {
		t.Fatalf("usage = %+v, want an empty rollup", usage)
	}
}

func TestPSCollectReadsTheExecsProcessTree(t *testing.T) {
	sampler := psSampler{
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
