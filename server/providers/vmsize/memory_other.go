//go:build !darwin && !linux && !windows

package vmsize

// physicalMemoryBytes cannot answer on a platform no local VM provider runs on;
// Host falls back to its fixed default there.
func physicalMemoryBytes() (uint64, bool) { return 0, false }
