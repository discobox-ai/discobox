package resources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

func TestCollectorCollectsProcAndCgroupData(t *testing.T) {
	root := t.TempDir()
	procRoot := filepath.Join(root, "proc")
	cgroupRoot := filepath.Join(root, "sys/fs/cgroup")
	procDir := filepath.Join(procRoot, "123")
	cgroupDir := filepath.Join(cgroupRoot, "sandbox/term")
	mkdirAll(t, procDir)
	mkdirAll(t, cgroupDir)
	writeFile(t, filepath.Join(procDir, "cgroup"), "0::/sandbox/term\n")
	writeFile(t, filepath.Join(procDir, "status"), "Name:\tcodex\nState:\tS (sleeping)\nPPid:\t1\n")
	writeFile(t, filepath.Join(procDir, "cmdline"), "codex\x00--resume\x00")
	writeFile(t, filepath.Join(procDir, "stat"), "123 (codex) S 1 2 3\n")
	writeFile(t, filepath.Join(cgroupDir, "cgroup.procs"), "123\n")
	writeFile(t, filepath.Join(cgroupDir, "memory.current"), "4096\n")
	writeFile(t, filepath.Join(cgroupDir, "cpu.stat"), "usage_usec 100\nuser_usec 70\nsystem_usec 30\n")

	sample, err := (Collector{ProcRoot: procRoot, CgroupRoot: cgroupRoot}).Collect(context.Background(), execs.Exec{
		ID:     "ex_1",
		Status: execs.StatusRunning,
		PID:    123,
		Unit:   "discobox-exec-ex_1",
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(sample.Data, &data); err != nil {
		t.Fatalf("decode sample data: %v", err)
	}
	cgroup := data["cgroup"].(map[string]any)
	files := cgroup["files"].(map[string]any)
	if files["memory.current"] != "4096" {
		t.Fatalf("memory.current = %#v", files["memory.current"])
	}
	processes := data["processes"].([]any)
	if len(processes) != 1 {
		t.Fatalf("processes = %#v", processes)
	}
	first := processes[0].(map[string]any)
	cmdline := first["cmdline"].([]any)
	if cmdline[0] != "codex" || cmdline[1] != "--resume" {
		t.Fatalf("cmdline = %#v", cmdline)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
