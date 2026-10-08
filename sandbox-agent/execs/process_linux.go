package execs

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

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

// bootID names this boot of the machine. A process's start ticks count from
// boot, so they identify it only within one: the boot id is what keeps a pid
// and tick pair from an earlier boot from matching one in this.
var bootID = sync.OnceValues(func() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
})

// inspectProcess reads a process's identity — the boot and the clock tick it
// started on, which no later process holding the same pid in the same boot
// can share — and whether it has already exited and waits only to be reaped.
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
	boot, err := bootID()
	if err != nil {
		return processInfo{}, err
	}
	return processInfo{
		identity: boot + ":" + fields[19],
		exited:   fields[0] == "Z" || fields[0] == "X",
	}, nil
}
