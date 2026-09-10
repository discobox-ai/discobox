package krunvm

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// DefaultHostResources sizes a pool VM from this machine.
func DefaultHostResources() HostResources {
	cpus := uint(runtime.NumCPU())
	if cpus > maxVCPUs {
		cpus = maxVCPUs
	}
	if cpus == 0 {
		cpus = 1
	}
	memory := hostMemoryBytes() / 2
	if memory < minMemoryMiB*1024*1024 {
		memory = minMemoryMiB * 1024 * 1024
	}
	return HostResources{CPUCount: cpus, MemoryBytes: memory}
}

// hostMemoryBytes reads MemTotal, which is the installed memory minus what the
// kernel reserved for itself — the right denominator, since the half this
// takes is meant to leave the other half usable.
func hostMemoryBytes() uint64 {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return fallbackMemoryBytes
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		kib, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || kib == 0 {
			break
		}
		return kib * 1024
	}
	return fallbackMemoryBytes
}
