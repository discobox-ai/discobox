//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris

package poolagent

func availableStorageBytes(string) int64 {
	return 0
}
