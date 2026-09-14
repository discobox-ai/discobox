//go:build linux

package vmsize

import "golang.org/x/sys/unix"

func physicalMemoryBytes() (uint64, bool) {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return 0, false
	}
	// Totalram is counted in units of Unit bytes, which is 1 on most machines
	// and larger on hosts whose memory would overflow the field otherwise.
	//nolint:unconvert // Totalram is uint64 on 64-bit Linux but uint32 on 32-bit.
	return uint64(info.Totalram) * uint64(info.Unit), true
}
