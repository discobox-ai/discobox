package poolagent

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// poolCgroupRoot is this container's own cgroup as a container with a private
// cgroup namespace sees it: its own cgroup, presented as the root. Docker
// defaults to a private namespace on cgroup v2, so the files here describe the
// pool container — the agent, buildkitd, the registry, and the proxy together —
// and nothing outside it.
const poolCgroupRoot = "/sys/fs/cgroup"

// cgroupUsage is the pool container's own consumption, read from its cgroup.
//
// It is what makes the pool's own services legible. Sandboxes are not the only
// thing running on a pool host: buildkitd, the pool registry, the proxy and the
// mediator all live in this container (ADR 0044), and on a pool mid-build
// BuildKit is plausibly the largest consumer of the lot.
//
// It measures those services and *not* the sandboxes. The sandboxes run under a
// nested container runtime, and their cgroups are not children of this one — a
// walk of this subtree on a live pool finds `buildkit/` and
// `system.slice/discobox-*.service` and no sandbox at all. So this figure is
// added to the per-sandbox figures to get the pool's load, never subtracted
// from (ADR 0071 §6).
type cgroupUsage struct {
	CPUUsageUsec       int64
	CPUUserUsec        int64
	CPUSystemUsec      int64
	MemoryCurrentBytes int64
	MemoryPeakBytes    int64
	MemoryLimitBytes   int64
}

// readCgroupUsage reads cgroup v2 counters from root. It reports false when
// there is no readable cgroup v2 hierarchy there — a cgroup v1 host, or a
// platform with no cgroups at all — rather than a convincing set of zeroes.
func readCgroupUsage(root string) (cgroupUsage, bool) {
	text, ok := readCgroupFile(filepath.Join(root, "cpu.stat"))
	if !ok {
		return cgroupUsage{}, false
	}
	var usage cgroupUsage
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
			usage.CPUUsageUsec, found = parsed, true
		case "user_usec":
			usage.CPUUserUsec = parsed
		case "system_usec":
			usage.CPUSystemUsec = parsed
		}
	}
	if !found {
		return cgroupUsage{}, false
	}
	usage.MemoryCurrentBytes, _ = readCgroupInt(filepath.Join(root, "memory.current"))
	usage.MemoryPeakBytes, _ = readCgroupInt(filepath.Join(root, "memory.peak"))
	// memory.max reads "max" when unlimited, which parses as no limit — the
	// same zero the field means.
	usage.MemoryLimitBytes, _ = readCgroupInt(filepath.Join(root, "memory.max"))
	return usage, true
}

func readCgroupFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func readCgroupInt(path string) (int64, bool) {
	text, ok := readCgroupFile(path)
	if !ok {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
