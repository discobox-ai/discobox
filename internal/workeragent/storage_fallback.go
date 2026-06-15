//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris

package workeragent

func availableStorageBytes(string) int64 {
	return 0
}
