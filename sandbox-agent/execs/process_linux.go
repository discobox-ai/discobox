package execs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// userHZ is the unit /proc reports a process's start time in: clock ticks,
// which the kernel fixes at 100 per second for everything it shows userspace.
const userHZ = 100

// processes lists every process the kernel knows of.
func processes() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]int, 0, len(entries))
	for _, entry := range entries {
		if pid, err := strconv.Atoi(entry.Name()); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out, nil
}

// inspectProcess reads when a process started and whether it has already
// exited and waits only to be reaped.
func inspectProcess(pid int) (processInfo, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return processInfo{}, err
	}
	// The command name is the second field and may hold spaces and
	// parentheses of its own, so the fields are read from after the last ')'.
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return processInfo{}, errors.New("malformed /proc stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	// fields[0] is the state (field 3 of stat(5)); fields[19] is starttime
	// (field 22), in clock ticks since boot.
	if len(fields) < 20 {
		return processInfo{}, errors.New("malformed /proc stat")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return processInfo{}, err
	}
	boot, err := bootTime()
	if err != nil {
		return processInfo{}, err
	}
	return processInfo{
		started: boot.Add(time.Duration(ticks) * time.Second / userHZ),
		exited:  fields[0] == "Z" || fields[0] == "X",
	}, nil
}

// bootTime is when the machine booted, from /proc/stat's btime: whole seconds,
// which is why a start time read from /proc is good to about a second.
func bootTime() (time.Time, error) {
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for line := range strings.SplitSeq(string(stat), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(seconds, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}
