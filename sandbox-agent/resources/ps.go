package resources

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
)

// psSampler is darwin's Sampler. A macOS sandbox is a whole guest, so the
// sandbox is every process in it, and its totals are the per-process rollup —
// there is no cgroup to charge against, which makes Source "proc".
//
// A sum over the processes alive now is not a cumulative counter: it falls
// whenever one exits, and the pool agent reads a counter that fell as no rate
// at all — a build of short-lived compilers would look idle exactly while it
// is busiest (ADR 0071). So this sampler keeps the one thing Linux's cgroup
// keeps for it: the CPU time of processes that have exited, each at its last
// sample, added to every total after. That makes the total monotonic. It still
// undercounts what a process spent after its last sample and every process
// that lived entirely between two, and it starts again from the live sum when
// the agent restarts — which a counter falling across a restart already means
// to the pool.
//
// Memory is the one total that is not the rollup. A guest's summed resident
// size counts the shared system libraries once per process, and a guest runs
// hundreds of them, so it can exceed the machine several times over. The whole
// machine is the sandbox, so its memory in use — everything but its free pages,
// page cache included, as a cgroup's memory.current includes it — is the
// direct answer, and the machine's size is its limit.
//
// A process's CPU time and resident size are behind libproc on darwin, which
// is cgo; ps is the system's own reader of them, so the sample runs it.
// Command and start time come from the kernel's process table instead, which
// is a plain sysctl, because ps can print neither in a form that can be told
// apart from the command line beside it.
//
// Only the running of ps and the sysctl are darwin's own; what they return is
// read here, on every platform, so its tests run everywhere.
type psSampler struct {
	// ps runs ps and returns its stdout.
	ps func(ctx context.Context) ([]byte, error)
	// kernel reads every process's command and start time.
	kernel func() (map[int]kernelProc, error)
	// memory reads the machine's memory in use and its size, in bytes.
	memory func() (current, limit int64, err error)

	// mu is held for a whole sample, ps included: two overlapping status
	// calls would otherwise record their processes out of order, counting
	// a process twice or a total below the one just reported.
	mu sync.Mutex
	// live is each process's CPU time at the last sample, by its identity.
	live map[processKey]cpuTime
	// exited is the CPU time of every process seen to exit, at its last
	// sample.
	exited cpuTime
}

// processKey identifies one process across samples: a PID alone is reused.
type processKey struct {
	pid        int
	startTicks uint64
}

// cpuTime is a cumulative CPU figure, in microseconds.
type cpuTime struct{ total, user int64 }

// psRow is one process as ps reports it.
type psRow struct {
	pid, ppid     int
	cpuUsec       int64
	userUsec      int64
	residentBytes int64
	virtualBytes  int64
	args          string
}

// kernelProc is what the kernel's process table says about one process.
type kernelProc struct {
	comm string
	// startTicks is when the process started, in Linux's 100 Hz ticks since
	// boot, so the field means on darwin what it means everywhere: with the
	// PID, the identity of one process across samples (ADR 0071 §3).
	startTicks uint64
}

func (s *psSampler) Sample(ctx context.Context) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	usage := Usage{ObservedAt: time.Now().UTC(), Source: "proc"}
	rows, err := s.rows(ctx)
	if err != nil {
		// No processes is what says "no sample": the status handler omits
		// a sample that counted none rather than report its zeroes, so the
		// pool keeps differencing against the last real one. The CPU
		// bookkeeping is untouched, so the next sample carries on from it.
		return usage
	}
	// A kernel table that cannot be read leaves every process without the
	// identity a candidate and the CPU bookkeeping need (see retire).
	kernel, err := s.kernel()
	if err != nil {
		kernel = nil
	}
	all := make([]ProcessUsage, 0, len(rows))
	live := make(map[processKey]cpuTime, len(rows))
	unidentified := map[int]cpuTime{}
	for _, row := range rows {
		cpu := cpuTime{total: row.cpuUsec, user: row.userUsec}
		usage.Memory.VirtualBytes += row.virtualBytes
		usage.Memory.ResidentBytes += row.residentBytes
		proc, ok := kernel[row.pid]
		if !ok {
			unidentified[row.pid] = cpu
			// Started or gone between ps and the sysctl: its memory and its
			// place in the process count are counted, but with no start time it
			// cannot be told from a process that reuses its PID, so it is no
			// candidate and its CPU is retire's to place.
			continue
		}
		live[processKey{pid: row.pid, startTicks: proc.startTicks}] = cpu
		all = append(all, ProcessUsage{
			PID:           row.pid,
			Command:       proc.comm,
			Cmdline:       row.args,
			StartTicks:    proc.startTicks,
			CPUUsec:       row.cpuUsec,
			VirtualBytes:  row.virtualBytes,
			ResidentBytes: row.residentBytes,
		})
	}
	s.retire(live, unidentified)
	// The total is what is remembered — live and exited — so that nothing
	// counted once can leave it.
	usage.CPU.UsageUsec, usage.CPU.UserUsec = s.exited.total, s.exited.user
	for _, cpu := range s.live {
		usage.CPU.UsageUsec += cpu.total
		usage.CPU.UserUsec += cpu.user
	}
	usage.CPU.SystemUsec = max(usage.CPU.UsageUsec-usage.CPU.UserUsec, 0)
	if current, limit, err := s.memory(); err == nil {
		usage.Memory.CurrentBytes = current
		usage.Memory.LimitBytes = limit
	} else {
		// The rollup's answer, as on a Linux sandbox with no cgroup to read.
		usage.Memory.CurrentBytes = usage.Memory.ResidentBytes
	}
	usage.ProcessCount = len(rows)
	usage.Processes = topCandidates(all)
	return usage
}

// retire records this sample's live processes and adds every process that
// has exited since the last sample to the exited total. s.mu is held.
//
// A process ps listed but the kernel's table did not name — the whole guest,
// when the table could not be read — has no identity this sample. One
// already remembered under its PID is carried forward rather than retired,
// at the larger of its two figures: if it is the same process its time only
// grew, and if another process has taken the PID the old one is retired at
// no less than its last figure on the next sample — more, and the new
// process's time counted twice, only if the newcomer has already outrun it;
// the total may overcount there but never falls. One not remembered is left
// out of the total until a sample can name it, so if it exits first it is
// the undercount of a process that lived between two samples, never a total
// that fell.
func (s *psSampler) retire(live map[processKey]cpuTime, unidentified map[int]cpuTime) {
	for key, last := range s.live {
		if _, ok := live[key]; ok {
			continue
		}
		if cpu, ok := unidentified[key.pid]; ok {
			live[key] = cpuTime{total: max(cpu.total, last.total), user: max(cpu.user, last.user)}
			continue
		}
		s.exited.total += last.total
		s.exited.user += last.user
	}
	s.live = live
}

// Collect reads the exec's process and every process descended from it: on
// darwin there is no cgroup to say which processes are the exec's, and its
// process tree is the nearest thing.
func (s *psSampler) Collect(ctx context.Context, ex execs.Exec) (store.ResourceSample, error) {
	sampledAt := time.Now().UTC()
	data := execSnapshot(ex)
	processes := []map[string]any{}
	if ex.PID > 0 {
		rows, err := s.rows(ctx)
		if err != nil {
			return store.ResourceSample{}, err
		}
		for _, row := range descendants(rows, int(ex.PID)) {
			processes = append(processes, map[string]any{
				"pid":           row.pid,
				"ppid":          row.ppid,
				"cpuUsec":       row.cpuUsec,
				"userUsec":      row.userUsec,
				"residentBytes": row.residentBytes,
				"virtualBytes":  row.virtualBytes,
				"args":          row.args,
			})
		}
	}
	data["processes"] = processes
	return resourceSample(ex, sampledAt, "darwin-ps", data)
}

func (s *psSampler) rows(ctx context.Context) ([]psRow, error) {
	out, err := s.ps(ctx)
	if err != nil {
		return nil, err
	}
	return parsePS(string(out)), nil
}

// descendants is root and every process under it, root first and each
// process before its children.
func descendants(rows []psRow, root int) []psRow {
	byPID := make(map[int]psRow, len(rows))
	children := map[int][]int{}
	for _, row := range rows {
		byPID[row.pid] = row
		children[row.ppid] = append(children[row.ppid], row.pid)
	}
	if _, ok := byPID[root]; !ok {
		return nil
	}
	var out []psRow
	seen := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		out = append(out, byPID[pid])
		queue = append(queue, children[pid]...)
	}
	return out
}

// psFixedColumns is how many columns come before the command line.
const psFixedColumns = 6

// parsePS reads ps's output for psArgs. A line that does not parse is skipped:
// a process can exit while ps is printing it.
func parsePS(out string) []psRow {
	var rows []psRow
	for line := range strings.Lines(out) {
		fields, args := cutColumns(strings.TrimRight(line, "\r\n"), psFixedColumns)
		if len(fields) < psFixedColumns {
			continue
		}
		pid, errPID := strconv.Atoi(fields[0])
		ppid, errPPID := strconv.Atoi(fields[1])
		cpu, okCPU := parseCPUTime(fields[2])
		user, okUser := parseCPUTime(fields[3])
		rss, errRSS := strconv.ParseInt(fields[4], 10, 64)
		vsz, errVSZ := strconv.ParseInt(fields[5], 10, 64)
		if errPID != nil || errPPID != nil || !okCPU || !okUser || errRSS != nil || errVSZ != nil {
			continue
		}
		rows = append(rows, psRow{
			pid:  pid,
			ppid: ppid,
			// ps's rss and vsz are in KiB.
			residentBytes: rss * 1024,
			virtualBytes:  vsz * 1024,
			cpuUsec:       cpu,
			userUsec:      user,
			args:          args,
		})
	}
	return rows
}

// cutColumns splits the first n whitespace-separated columns off line and
// returns them and the rest of the line as it was printed, so a command line's
// own spacing survives.
func cutColumns(line string, n int) ([]string, string) {
	fields := make([]string, 0, n)
	rest := line
	for len(fields) < n {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			break
		}
		end := strings.IndexAny(rest, " \t")
		if end < 0 {
			end = len(rest)
		}
		fields = append(fields, rest[:end])
		rest = rest[end:]
	}
	return fields, strings.TrimLeft(rest, " \t")
}

// parseCPUTime reads a CPU time as BSD ps prints it — minutes, seconds and
// hundredths ("12:34.56"), with hours or days in front when it runs that long
// ("1:02:03.45", "2-01:02:03") — as microseconds.
func parseCPUTime(text string) (int64, bool) {
	var days int64
	if before, after, ok := strings.Cut(text, "-"); ok {
		parsed, err := strconv.ParseInt(before, 10, 64)
		if err != nil || parsed < 0 {
			return 0, false
		}
		days, text = parsed, after
	}
	parts := strings.Split(text, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	whole, fraction, _ := strings.Cut(parts[len(parts)-1], ".")
	parts[len(parts)-1] = whole
	var seconds int64
	for _, part := range parts {
		value, err := strconv.ParseInt(part, 10, 64)
		if err != nil || value < 0 {
			return 0, false
		}
		seconds = seconds*60 + value
	}
	usec := (days*86400 + seconds) * 1_000_000
	if fraction != "" {
		if len(fraction) > 6 {
			fraction = fraction[:6]
		}
		value, err := strconv.ParseInt(fraction+strings.Repeat("0", 6-len(fraction)), 10, 64)
		if err != nil || value < 0 {
			return 0, false
		}
		usec += value
	}
	return usec, true
}
