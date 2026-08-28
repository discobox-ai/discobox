//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !solaris

package poolagent

func filesystemUsage(string) (FilesystemUsage, bool) {
	return FilesystemUsage{}, false
}
