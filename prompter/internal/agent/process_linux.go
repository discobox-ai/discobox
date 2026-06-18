//go:build linux

package agent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// CollectAncestry returns sanitized parent process identity from /proc. It is a
// best-effort local probe and never reads ancestor environment values.
func CollectAncestry(pid int) ([]Process, error) {
	var ancestry []Process
	seen := map[int]bool{}
	current := pid

	for current > 1 {
		if seen[current] {
			return ancestry, fmt.Errorf("process ancestry loop at pid %d", current)
		}
		seen[current] = true

		process, err := readProcess(current)
		if err != nil {
			if current == pid {
				return nil, err
			}
			return ancestry, nil
		}
		if current != pid {
			ancestry = append(ancestry, process)
		}
		current = process.PPID
	}

	return ancestry, nil
}

func readProcess(pid int) (Process, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return Process{}, err
	}

	process := Process{PID: pid}
	for _, line := range strings.Split(string(status), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Name":
			process.Comm = value
		case "PPid":
			ppid, err := strconv.Atoi(value)
			if err != nil {
				return Process{}, fmt.Errorf("parse parent pid for %d: %w", pid, err)
			}
			process.PPID = ppid
		}
	}

	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		process.Exe = exe
	}

	return process, nil
}
