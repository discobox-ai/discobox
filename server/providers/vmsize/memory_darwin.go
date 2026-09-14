//go:build darwin

package vmsize

import "golang.org/x/sys/unix"

func physicalMemoryBytes() (uint64, bool) {
	// hw.memsize is the machine's physical memory. There is no portable Go way
	// to ask, and shelling out to sysctl(8) to learn a constant would be worse.
	memory, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, false
	}
	return memory, true
}
