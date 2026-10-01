package resources

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
)

// psSampler is darwin's Sampler. A macOS sandbox is a whole guest, so the
// sandbox is every process in it, and its totals are the per-process rollup —
// there is no cgroup to charge against, which makes Source "proc" and
// CurrentBytes the summed resident size, exactly as on a Linux sandbox whose
// cgroup cannot be read.
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
}

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

func (s psSampler) Sample(ctx context.Context) Usage {
	usage := Usage{ObservedAt: time.Now().UTC(), Source: "proc"}
	rows, err := s.rows(ctx)
	if err != nil {
		return usage
	}
	// A kernel table that cannot be read leaves the totals true and the
	// candidates without the identity a candidate needs, so it names none.
	kernel, err := s.kernel()
	if err != nil {
		kernel = nil
	}
	all := make([]ProcessUsage, 0, len(rows))
	for _, row := range rows {
		usage.CPU.UsageUsec += row.cpuUsec
		usage.CPU.UserUsec += row.userUsec
		usage.Memory.VirtualBytes += row.virtualBytes
		usage.Memory.ResidentBytes += row.residentBytes
		proc, ok := kernel[row.pid]
		if !ok {
			// Started or gone between ps and the sysctl: counted, but with no
			// start time it cannot be told from a process that reuses its PID.
			continue
		}
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
	usage.CPU.SystemUsec = max(usage.CPU.UsageUsec-usage.CPU.UserUsec, 0)
	usage.Memory.CurrentBytes = usage.Memory.ResidentBytes
	usage.ProcessCount = len(rows)
	usage.Processes = topCandidates(all)
	return usage
}

// Collect reads the exec's process and every process descended from it: on
// darwin there is no cgroup to say which processes are the exec's, and its
// process tree is the nearest thing.
func (s psSampler) Collect(ctx context.Context, ex execs.Exec) (store.ResourceSample, error) {
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

func (s psSampler) rows(ctx context.Context) ([]psRow, error) {
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
