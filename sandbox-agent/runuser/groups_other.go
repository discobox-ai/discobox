//go:build !darwin && !windows

package runuser

// maxGroups is Linux's NGROUPS_MAX.
const maxGroups = 65536
