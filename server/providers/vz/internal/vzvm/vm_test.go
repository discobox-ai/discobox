package vzvm

import (
	"strings"
	"testing"
)

func validOptions() Options {
	return Options{
		CPUCount:       1,
		MemoryBytes:    512 * 1024 * 1024,
		KernelPath:     "/guest/vmlinux",
		RootImagePath:  "/guest/root.ext4",
		DataImagePath:  "/pool/data.raw",
		CacheImagePath: "/pool/cache.raw",
	}
}

// A share is rejected here rather than at the framework boundary, where an
// over-long or duplicated tag is an opaque configuration rejection with nothing
// naming the share that caused it.
func TestValidateRejectsUnusableShares(t *testing.T) {
	for name, shares := range map[string][]SharedDirectory{
		"no tag":         {{HostPath: "/Users"}},
		"reserved tag":   {{Tag: "..", HostPath: "/Users"}},
		"over-long tag":  {{Tag: strings.Repeat("t", maxSharedDirectoryTagLen+1), HostPath: "/Users"}},
		"relative path":  {{Tag: "users", HostPath: "Users"}},
		"no path":        {{Tag: "users"}},
		"duplicated tag": {{Tag: "users", HostPath: "/Users"}, {Tag: "users", HostPath: "/opt"}},
	} {
		opts := validOptions()
		opts.SharedDirectories = shares
		if err := opts.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, shares)
		}
	}
}

func TestValidateAcceptsAReadOnlyShare(t *testing.T) {
	opts := validOptions()
	opts.SharedDirectories = []SharedDirectory{{Tag: "discobox-users", HostPath: "/Users", ReadOnly: true}}
	if err := opts.Validate(); err != nil {
		t.Fatalf("Validate rejected a well-formed share: %v", err)
	}
}
