package resources

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/store"
)

// Collect reads the exec's cgroup files and every process in that cgroup.
func (c Procfs) Collect(ctx context.Context, ex execs.Exec) (store.ResourceSample, error) {
	c = c.normalized()
	sampledAt := time.Now().UTC()
	data := execSnapshot(ex)
	if ex.PID > 0 {
		pid := int(ex.PID)
		cgroupPath := c.cgroupPath(pid)
		data["cgroup"] = map[string]any{
			"path":  cgroupPath,
			"files": c.readCgroupFiles(cgroupPath),
		}
		data["processes"] = c.execProcesses(pid, cgroupPath)
	} else {
		data["cgroup"] = map[string]any{"files": map[string]any{}}
		data["processes"] = []any{}
	}
	select {
	case <-ctx.Done():
		return store.ResourceSample{}, ctx.Err()
	default:
	}
	return resourceSample(ex, sampledAt, "linux-procfs-cgroup", data)
}

func (c Procfs) cgroupPath(pid int) string {
	data, err := os.ReadFile(filepath.Join(c.ProcRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] == "0" {
			return strings.TrimSpace(parts[2])
		}
	}
	return ""
}

func (c Procfs) readCgroupFiles(cgroupPath string) map[string]any {
	out := map[string]any{}
	if cgroupPath == "" {
		return out
	}
	root := filepath.Clean(filepath.Join(c.CgroupRoot, strings.TrimPrefix(cgroupPath, "/")))
	for _, name := range []string{
		"cgroup.controllers",
		"cgroup.events",
		"cgroup.procs",
		"cgroup.stat",
		"cgroup.threads",
		"cpu.max",
		"cpu.pressure",
		"cpu.stat",
		"io.pressure",
		"io.stat",
		"memory.current",
		"memory.events",
		"memory.max",
		"memory.peak",
		"memory.pressure",
		"memory.stat",
		"pids.current",
		"pids.events",
		"pids.max",
	} {
		if value, ok := readTrimmed(filepath.Join(root, name)); ok {
			out[name] = parseResourceText(value)
		}
	}
	return out
}

func (c Procfs) execProcesses(rootPID int, cgroupPath string) []map[string]any {
	seen := map[int]bool{}
	pids := []int{rootPID}
	pids = append(pids, c.cgroupPIDs(cgroupPath)...)
	out := make([]map[string]any, 0, len(pids))
	for _, pid := range pids {
		if pid <= 0 || seen[pid] {
			continue
		}
		seen[pid] = true
		if proc := c.processSnapshot(pid); proc != nil {
			out = append(out, proc)
		}
	}
	return out
}

func (c Procfs) cgroupPIDs(cgroupPath string) []int {
	if cgroupPath == "" {
		return nil
	}
	data, ok := readTrimmed(filepath.Join(c.CgroupRoot, strings.TrimPrefix(cgroupPath, "/"), "cgroup.procs"))
	if !ok {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(data) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func (c Procfs) processSnapshot(pid int) map[string]any {
	root := filepath.Join(c.ProcRoot, strconv.Itoa(pid))
	statusText, ok := readTrimmed(filepath.Join(root, "status"))
	if !ok {
		return nil
	}
	proc := map[string]any{
		"pid":    pid,
		"status": parseKeyValueLines(statusText),
	}
	if cmdline, ok := os.ReadFile(filepath.Join(root, "cmdline")); ok == nil {
		proc["cmdline"] = splitNUL(cmdline)
	}
	if statm, ok := readTrimmed(filepath.Join(root, "statm")); ok {
		proc["statm"] = strings.Fields(statm)
	}
	if ioText, ok := readTrimmed(filepath.Join(root, "io")); ok {
		proc["io"] = parseKeyValueLines(ioText)
	}
	if statText, ok := readTrimmed(filepath.Join(root, "stat")); ok {
		proc["stat"] = parseStat(statText)
	}
	return proc
}

func readTrimmed(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func parseResourceText(value string) any {
	lines := strings.Split(value, "\n")
	if len(lines) == 1 {
		fields := strings.Fields(value)
		if len(fields) == 1 {
			return fields[0]
		}
		return fields
	}
	parsed := make([]any, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parsed = append(parsed, parseKeyValueFields(line))
	}
	return parsed
}

func parseKeyValueFields(line string) map[string]string {
	out := map[string]string{}
	for _, field := range strings.Fields(line) {
		key, value, ok := strings.Cut(field, "=")
		if ok {
			out[key] = value
			continue
		}
		if _, exists := out["value"]; !exists {
			out["value"] = field
		}
	}
	return out
}

func parseKeyValueLines(text string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok {
			out[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return out
}

func parseStat(text string) map[string]any {
	fields := strings.Fields(text)
	if len(fields) < 4 {
		return map[string]any{"raw": text}
	}
	return map[string]any{
		"raw":   text,
		"comm":  strings.Trim(fields[1], "()"),
		"state": fields[2],
		"ppid":  fields[3],
	}
}

func splitNUL(data []byte) []string {
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}
