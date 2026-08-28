package resources

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProc lays down a synthetic /proc entry. The stat line is the real
// format: pid, (comm), state, then the numbered fields.
func writeProc(t *testing.T, root string, pid int, comm string, utime, stime, starttime, vsize uint64, rssPages int64, cmdline string) {
	t.Helper()
	dir := filepath.Join(root, itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Fields 3..13 are placeholders; utime is 14 and stime 15.
	stat := itoa(pid) + " (" + comm + ") S 1 1 1 0 -1 4194304 100 0 0 0 " +
		utoa(utime) + " " + utoa(stime) + " 0 0 20 0 5 0 " +
		utoa(starttime) + " " + utoa(vsize) + " " + utoa(uint64(rssPages)) + " 0 0 0 0 0"
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoa(v int) string { return utoa(uint64(v)) }
func utoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func writeCgroup(t *testing.T, root string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSampleReadsCgroupTotalsAndProcessRollup(t *testing.T) {
	procRoot := t.TempDir()
	cgroupRoot := t.TempDir()
	// 4096-byte pages keeps the arithmetic checkable by hand.
	const pageSize = 4096

	writeProc(t, procRoot, 100, "node", 300, 100, 55, 2<<30, 1000, "node\x00server.js\x00")
	writeProc(t, procRoot, 101, "gopls", 10, 5, 60, 1<<30, 500, "gopls\x00")
	writeCgroup(t, cgroupRoot, map[string]string{
		"cpu.stat":       "usage_usec 8204113\nuser_usec 6000000\nsystem_usec 2204113\nnr_periods 0\n",
		"cpu.max":        "max 100000\n",
		"memory.current": "6442450944\n",
		"memory.peak":    "7000000000\n",
		"memory.max":     "max\n",
		"memory.stat":    "anon 4000000000\nfile 2000000000\nslab 1234\n",
	})

	usage := Sampler{ProcRoot: procRoot, CgroupRoot: cgroupRoot, PageSize: pageSize}.Sample()

	if usage.Source != "cgroup" {
		t.Fatalf("source = %q, want cgroup", usage.Source)
	}
	if usage.CPU.UsageUsec != 8204113 {
		t.Errorf("usageUsec = %d, want 8204113", usage.CPU.UsageUsec)
	}
	// cpu.max reads "max", so the sandbox is unlimited — which is what a
	// sandbox container really is today.
	if usage.CPU.LimitVCPUs != 0 {
		t.Errorf("limitVcpus = %v, want 0 for an unlimited cgroup", usage.CPU.LimitVCPUs)
	}
	if usage.Memory.CurrentBytes != 6442450944 {
		t.Errorf("currentBytes = %d", usage.Memory.CurrentBytes)
	}
	if usage.Memory.LimitBytes != 0 {
		t.Errorf("limitBytes = %d, want 0 for an unlimited cgroup", usage.Memory.LimitBytes)
	}
	if usage.Memory.AnonBytes != 4000000000 || usage.Memory.FileBytes != 2000000000 {
		t.Errorf("anon/file = %d/%d", usage.Memory.AnonBytes, usage.Memory.FileBytes)
	}
	// The rollup is the processes' own view and is independent of the cgroup.
	if want := int64(2<<30) + int64(1<<30); usage.Memory.VirtualBytes != want {
		t.Errorf("virtualBytes = %d, want %d", usage.Memory.VirtualBytes, want)
	}
	if want := int64(1500) * pageSize; usage.Memory.ResidentBytes != want {
		t.Errorf("residentBytes = %d, want %d", usage.Memory.ResidentBytes, want)
	}
	if usage.ProcessCount != 2 {
		t.Errorf("processCount = %d, want 2", usage.ProcessCount)
	}
}

func TestSampleFallsBackToProcWithoutCgroup(t *testing.T) {
	procRoot := t.TempDir()
	writeProc(t, procRoot, 7, "sh", 100, 50, 5, 1<<20, 64, "sh\x00")

	usage := Sampler{ProcRoot: procRoot, CgroupRoot: filepath.Join(t.TempDir(), "absent"), PageSize: 4096}.Sample()

	if usage.Source != "proc" {
		t.Fatalf("source = %q, want proc", usage.Source)
	}
	// 150 ticks at 100Hz is 1.5s of CPU.
	if want := int64(1_500_000); usage.CPU.UsageUsec != want {
		t.Errorf("usageUsec = %d, want %d", usage.CPU.UsageUsec, want)
	}
	// With no cgroup to charge against, resident size stands in for "what this
	// sandbox costs" rather than reporting zero.
	if usage.Memory.CurrentBytes != usage.Memory.ResidentBytes {
		t.Errorf("currentBytes = %d, want it to fall back to resident %d",
			usage.Memory.CurrentBytes, usage.Memory.ResidentBytes)
	}
}

// A command containing spaces and parentheses is the case that breaks a naive
// Fields() parse, and a process can be named anything.
func TestParseProcStatHandlesCommandWithSpacesAndParens(t *testing.T) {
	stat := "4242 (my (weird) name) S 1 1 1 0 -1 4194304 100 0 0 0 " +
		"700 300 0 0 20 0 5 0 9911 1048576 128 0 0 0 0 0"
	parsed, ok := parseProcStat(stat)
	if !ok {
		t.Fatal("parse failed")
	}
	if parsed.pid != 4242 {
		t.Errorf("pid = %d", parsed.pid)
	}
	if parsed.comm != "my (weird) name" {
		t.Errorf("comm = %q", parsed.comm)
	}
	if parsed.utime != 700 || parsed.stime != 300 {
		t.Errorf("utime/stime = %d/%d", parsed.utime, parsed.stime)
	}
	if parsed.starttime != 9911 {
		t.Errorf("starttime = %d", parsed.starttime)
	}
	if parsed.vsize != 1048576 || parsed.rssPages != 128 {
		t.Errorf("vsize/rss = %d/%d", parsed.vsize, parsed.rssPages)
	}
}

func TestTopCandidatesUnionsBothRankings(t *testing.T) {
	// One process is the busiest and holds almost nothing; another holds the
	// most memory and has burned almost no CPU. Ranking on either axis alone
	// would drop one of them.
	var all []ProcessUsage
	for i := range processCandidates + 5 {
		all = append(all, ProcessUsage{PID: i + 1, CPUUsec: int64(i), ResidentBytes: int64(i)})
	}
	busiest := ProcessUsage{PID: 9001, CPUUsec: 1 << 40, ResidentBytes: 1}
	largest := ProcessUsage{PID: 9002, CPUUsec: 1, ResidentBytes: 1 << 40}
	all = append(all, busiest, largest)

	got := topCandidates(all)

	var sawBusiest, sawLargest bool
	for _, proc := range got {
		sawBusiest = sawBusiest || proc.PID == busiest.PID
		sawLargest = sawLargest || proc.PID == largest.PID
	}
	if !sawBusiest {
		t.Error("busiest process missing from candidates")
	}
	if !sawLargest {
		t.Error("largest process missing from candidates")
	}
	if len(got) > processCandidates*2 {
		t.Errorf("returned %d candidates, want at most %d", len(got), processCandidates*2)
	}
	seen := map[int]bool{}
	for _, proc := range got {
		if seen[proc.PID] {
			t.Fatalf("pid %d appears twice", proc.PID)
		}
		seen[proc.PID] = true
	}
}
