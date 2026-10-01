//go:build !darwin

package resources

import "os"

// NewSampler is Linux's: this container's cgroup and /proc. Windows has no
// sandbox of its own yet, and finding neither there reports nothing.
func NewSampler() Sampler {
	return Procfs{ProcRoot: "/proc", CgroupRoot: "/sys/fs/cgroup", PageSize: os.Getpagesize()}
}
