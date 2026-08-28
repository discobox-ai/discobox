package resources

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clockTicksPerSecond is the USER_HZ the kernel reports process CPU times in.
// It is a compile-time constant of the kernel ABI on every architecture Linux
// runs a sandbox on, and is 100 on all of them; reading it properly means
// sysconf(_SC_CLK_TCK), which is cgo. Only the per-process counters are in
// these units — the cgroup reports microseconds directly.
const clockTicksPerSecond = 100

// processCandidates bounds how many processes one sample names. The list is
// the union of the busiest by cumulative CPU and the largest by resident size,
// so a sandbox with fewer distinct offenders in each ranking reports fewer.
//
// It is a candidate list, not an answer: whoever differences two samples ranks
// them by rate and keeps far fewer (ADR 0071 §3).
const processCandidates = 15

// Usage is the whole sandbox's resource consumption at one moment.
//
// Every CPU figure here is a cumulative counter, never a rate. What "busy"
// means is the difference between two samples divided by the time between
// them, and the component that holds two samples is the pool agent, which
// polls every sandbox in the pool on one tick and so measures all of them over
// the same window (ADR 0071 §§1-2).
type Usage struct {
	ObservedAt time.Time `json:"observedAt"`
	// Source names where the totals came from: "cgroup" when this sandbox's
	// own cgroup was readable, "proc" when the per-process rollup had to
	// stand in for it. A consumer that cares about page cache should check
	// it, because the rollup cannot see any.
	Source       string         `json:"source"`
	CPU          CPUUsage       `json:"cpu"`
	Memory       MemoryUsage    `json:"memory"`
	ProcessCount int            `json:"processCount"`
	Processes    []ProcessUsage `json:"processes,omitempty"`
}

// CPUUsage is cumulative CPU time charged to the sandbox.
type CPUUsage struct {
	UsageUsec  int64 `json:"usageUsec"`
	UserUsec   int64 `json:"userUsec"`
	SystemUsec int64 `json:"systemUsec"`
	// LimitVCPUs is the cgroup's own quota in whole-CPU units, zero when the
	// cgroup is unlimited. Sandbox containers are created with no CPU limit
	// today, so this is normally zero and a rate has no ceiling to be read
	// against (ADR 0071 context).
	LimitVCPUs float64 `json:"limitVcpus,omitempty"`
}

// MemoryUsage carries two different true answers, and neither substitutes for
// the other (ADR 0071 §4).
//
// CurrentBytes is what the host charges this sandbox: anonymous memory, page
// cache, and kernel memory together. VirtualBytes and ResidentBytes are what
// the sandbox's own processes hold, summed, and so double-count every shared
// page — summed resident routinely exceeds CurrentBytes, which is expected.
type MemoryUsage struct {
	CurrentBytes  int64 `json:"currentBytes"`
	PeakBytes     int64 `json:"peakBytes,omitempty"`
	AnonBytes     int64 `json:"anonBytes,omitempty"`
	FileBytes     int64 `json:"fileBytes,omitempty"`
	LimitBytes    int64 `json:"limitBytes,omitempty"`
	VirtualBytes  int64 `json:"virtualBytes"`
	ResidentBytes int64 `json:"residentBytes"`
}

// ProcessUsage is one candidate process, reported with cumulative counters for
// the same reason the sandbox totals are.
type ProcessUsage struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Cmdline string `json:"cmdline,omitempty"`
	// StartTicks is the process's start time in kernel ticks since boot. It is
	// part of this process's identity, not decoration: PIDs are reused, and
	// differencing a recycled PID against its predecessor's counter would
	// difference into a nonsense spike (ADR 0071 §3).
	StartTicks    uint64 `json:"startTicks"`
	CPUUsec       int64  `json:"cpuUsec"`
	VirtualBytes  int64  `json:"virtualBytes"`
	ResidentBytes int64  `json:"residentBytes"`
}

// Sampler reads one sandbox-wide usage sample. It holds no state between
// samples: sandbox-agent's status is computed fresh on every call and never
// pushed on its own initiative (ADR 0030), and the differencing that turns
// these counters into rates belongs to the pool agent.
type Sampler struct {
	ProcRoot   string
	CgroupRoot string
	// PageSize is the size of a page in bytes, for converting the page counts
	// /proc reports. Zero uses the running system's.
	PageSize int
}

func NewSampler() Sampler {
	return Sampler{ProcRoot: "/proc", CgroupRoot: "/sys/fs/cgroup", PageSize: os.Getpagesize()}
}

func (s Sampler) normalized() Sampler {
	if s.ProcRoot == "" {
		s.ProcRoot = "/proc"
	}
	if s.CgroupRoot == "" {
		s.CgroupRoot = "/sys/fs/cgroup"
	}
	if s.PageSize <= 0 {
		s.PageSize = os.Getpagesize()
	}
	return s
}

// Sample reads the sandbox's totals and its candidate processes.
//
// The cgroup read is the authoritative total when it succeeds. Inside a
// container with a private cgroup namespace — Docker's default on cgroup v2 —
// CgroupRoot is this container's own cgroup presented as the root, so the
// files there describe exactly this sandbox and nothing else. When they cannot
// be read the per-process rollup stands in, which sees no page cache and no
// kernel memory; Source records which happened.
func (s Sampler) Sample() Usage {
	s = s.normalized()
	usage := Usage{ObservedAt: time.Now().UTC(), Source: "proc"}

	procs, count := s.processes()
	usage.ProcessCount = count
	usage.Processes = procs
	// Summed over every process, not over the trimmed candidate list above:
	// these are the sandbox's totals, and the candidates are a ranking.
	usage.Memory.VirtualBytes, usage.Memory.ResidentBytes = s.memoryRollup()

	if cpu, ok := s.cgroupCPU(); ok {
		usage.CPU = cpu
		usage.Source = "cgroup"
	} else {
		usage.CPU = s.procCPU()
	}
	if memory, ok := s.cgroupMemory(); ok {
		usage.Memory.CurrentBytes = memory.CurrentBytes
		usage.Memory.PeakBytes = memory.PeakBytes
		usage.Memory.AnonBytes = memory.AnonBytes
		usage.Memory.FileBytes = memory.FileBytes
		usage.Memory.LimitBytes = memory.LimitBytes
	} else {
		// No cgroup to charge against, so the closest thing to "what this
		// sandbox costs" is what its processes resident-hold.
		usage.Memory.CurrentBytes = usage.Memory.ResidentBytes
	}
	return usage
}

func (s Sampler) cgroupCPU() (CPUUsage, bool) {
	text, ok := readTrimmed(filepath.Join(s.CgroupRoot, "cpu.stat"))
	if !ok {
		return CPUUsage{}, false
	}
	var cpu CPUUsage
	var found bool
	for _, line := range strings.Split(text, "\n") {
		key, value, cut := strings.Cut(strings.TrimSpace(line), " ")
		if !cut {
			continue
		}
		parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "usage_usec":
			cpu.UsageUsec, found = parsed, true
		case "user_usec":
			cpu.UserUsec = parsed
		case "system_usec":
			cpu.SystemUsec = parsed
		}
	}
	if !found {
		return CPUUsage{}, false
	}
	cpu.LimitVCPUs = s.cgroupCPULimit()
	return cpu, true
}

// cgroupCPULimit reads cpu.max, which is "<quota> <period>" or "max <period>"
// when unlimited. Zero means unlimited, which is what a sandbox container has.
func (s Sampler) cgroupCPULimit() float64 {
	text, ok := readTrimmed(filepath.Join(s.CgroupRoot, "cpu.max"))
	if !ok {
		return 0
	}
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	quota, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0
	}
	period, err := strconv.ParseFloat(fields[1], 64)
	if err != nil || period <= 0 {
		return 0
	}
	return quota / period
}

func (s Sampler) cgroupMemory() (MemoryUsage, bool) {
	current, ok := readInt64(filepath.Join(s.CgroupRoot, "memory.current"))
	if !ok {
		return MemoryUsage{}, false
	}
	memory := MemoryUsage{CurrentBytes: current}
	if peak, ok := readInt64(filepath.Join(s.CgroupRoot, "memory.peak")); ok {
		memory.PeakBytes = peak
	}
	// memory.max is "max" when unlimited, which readInt64 rejects, leaving
	// zero — the same "no ceiling" the field documents.
	if limit, ok := readInt64(filepath.Join(s.CgroupRoot, "memory.max")); ok {
		memory.LimitBytes = limit
	}
	if text, ok := readTrimmed(filepath.Join(s.CgroupRoot, "memory.stat")); ok {
		for _, line := range strings.Split(text, "\n") {
			key, value, cut := strings.Cut(strings.TrimSpace(line), " ")
			if !cut {
				continue
			}
			parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				continue
			}
			switch key {
			case "anon":
				memory.AnonBytes = parsed
			case "file":
				memory.FileBytes = parsed
			}
		}
	}
	return memory, true
}

// procCPU sums every process's CPU time, for a sandbox whose own cgroup is not
// readable. It undercounts against the cgroup, which keeps charging for
// processes that have already exited.
func (s Sampler) procCPU() CPUUsage {
	var cpu CPUUsage
	s.eachProcess(func(stat procStat, _ string) {
		cpu.UserUsec += ticksToUsec(stat.utime)
		cpu.SystemUsec += ticksToUsec(stat.stime)
	})
	cpu.UsageUsec = cpu.UserUsec + cpu.SystemUsec
	return cpu
}

func (s Sampler) memoryRollup() (virtual, resident int64) {
	s.eachProcess(func(stat procStat, _ string) {
		virtual += int64(stat.vsize)
		resident += stat.rssPages * int64(s.PageSize)
	})
	return virtual, resident
}

// processes returns the candidate list and the total number of processes seen.
func (s Sampler) processes() ([]ProcessUsage, int) {
	var all []ProcessUsage
	s.eachProcess(func(stat procStat, cmdline string) {
		all = append(all, ProcessUsage{
			PID:           stat.pid,
			Command:       stat.comm,
			Cmdline:       cmdline,
			StartTicks:    stat.starttime,
			CPUUsec:       ticksToUsec(stat.utime + stat.stime),
			VirtualBytes:  int64(stat.vsize),
			ResidentBytes: stat.rssPages * int64(s.PageSize),
		})
	})
	return topCandidates(all), len(all)
}

// topCandidates is the union of the busiest by cumulative CPU and the largest
// by resident size, in that order, deduplicated by PID.
func topCandidates(all []ProcessUsage) []ProcessUsage {
	if len(all) <= processCandidates {
		return all
	}
	byCPU := make([]ProcessUsage, len(all))
	copy(byCPU, all)
	sort.SliceStable(byCPU, func(i, j int) bool { return byCPU[i].CPUUsec > byCPU[j].CPUUsec })
	byRSS := make([]ProcessUsage, len(all))
	copy(byRSS, all)
	sort.SliceStable(byRSS, func(i, j int) bool { return byRSS[i].ResidentBytes > byRSS[j].ResidentBytes })

	seen := make(map[int]bool, processCandidates*2)
	out := make([]ProcessUsage, 0, processCandidates*2)
	for _, ranked := range [][]ProcessUsage{byCPU, byRSS} {
		for i := 0; i < processCandidates && i < len(ranked); i++ {
			if seen[ranked[i].PID] {
				continue
			}
			seen[ranked[i].PID] = true
			out = append(out, ranked[i])
		}
	}
	return out
}

// eachProcess walks /proc once, calling fn for every process whose stat could
// be read. A process that exits mid-walk simply does not appear; that is
// ordinary, not an error.
func (s Sampler) eachProcess(fn func(procStat, string)) {
	entries, err := os.ReadDir(s.ProcRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		text, ok := readTrimmed(filepath.Join(s.ProcRoot, entry.Name(), "stat"))
		if !ok {
			continue
		}
		stat, ok := parseProcStat(text)
		if !ok {
			continue
		}
		var cmdline string
		if data, err := os.ReadFile(filepath.Join(s.ProcRoot, entry.Name(), "cmdline")); err == nil {
			cmdline = strings.Join(splitNUL(data), " ")
		}
		fn(stat, cmdline)
	}
}

// procStat is the handful of /proc/<pid>/stat fields this package needs.
type procStat struct {
	pid       int
	comm      string
	utime     uint64
	stime     uint64
	starttime uint64
	vsize     uint64
	rssPages  int64
}

// parseProcStat reads the fields by position after the command name.
//
// The command is the reason this cannot simply be Fields(): it is the process's
// own executable name, unsanitized, so it may contain spaces and parentheses
// alike. The kernel wraps it in parentheses, so the split point is the *last*
// ')' in the line — a program named "my (weird) name" parses correctly only
// that way.
func parseProcStat(text string) (procStat, bool) {
	commStart := strings.IndexByte(text, '(')
	commEnd := strings.LastIndexByte(text, ')')
	if commStart < 0 || commEnd < commStart {
		return procStat{}, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(text[:commStart]))
	if err != nil {
		return procStat{}, false
	}
	stat := procStat{pid: pid, comm: text[commStart+1 : commEnd]}

	// Fields after the command name, 1-indexed as `man 5 proc` numbers them:
	// rest[0] is field 3 (state), so field N is rest[N-3].
	rest := strings.Fields(text[commEnd+1:])
	const (
		fieldUtime     = 14
		fieldStime     = 15
		fieldStarttime = 22
		fieldVsize     = 23
		fieldRSS       = 24
	)
	at := func(field int) (uint64, bool) {
		index := field - 3
		if index < 0 || index >= len(rest) {
			return 0, false
		}
		value, err := strconv.ParseUint(rest[index], 10, 64)
		if err != nil {
			return 0, false
		}
		return value, true
	}
	var ok bool
	if stat.utime, ok = at(fieldUtime); !ok {
		return procStat{}, false
	}
	if stat.stime, ok = at(fieldStime); !ok {
		return procStat{}, false
	}
	stat.starttime, _ = at(fieldStarttime)
	stat.vsize, _ = at(fieldVsize)
	// RSS is signed in the kernel's own accounting but never meaningfully
	// negative; a value that does not fit is reported as no pages rather than
	// as a wrapped one.
	if pages, ok := at(fieldRSS); ok && pages <= uint64(1)<<62 {
		stat.rssPages = int64(pages)
	}
	return stat, true
}

func ticksToUsec(ticks uint64) int64 {
	return int64(ticks) * 1_000_000 / clockTicksPerSecond
}

func readInt64(path string) (int64, bool) {
	text, ok := readTrimmed(path)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
