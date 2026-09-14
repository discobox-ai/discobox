package wslcsession

import "time"

// Options configures a new Session. Zero values pick reasonable defaults.
//
// This type has no build constraint - it's shared between the real
// (windows-only) implementation and the stub used on every other platform,
// so code that constructs an Options value compiles the same way regardless
// of target OS.
type Options struct {
	// DisplayName identifies the session; if empty, a name derived from this
	// process's PID is generated so concurrent runs don't collide.
	DisplayName string

	// ReplaceExisting makes a session already registered under DisplayName
	// something to end rather than an error: NewSession terminates it and
	// starts this one in its place.
	//
	// The session it ends is usually a VM whose process was killed rather than
	// closed - the service keeps its name until it notices, which takes seconds
	// to minutes - but it need not be: the service hands back any session of
	// that name, a live process's included. So it is off by default, and a
	// caller sets it only for a name it alone owns. Without it, a taken name is
	// ErrSessionExists.
	ReplaceExisting bool
	CPUCount        uint32
	MemoryMB        uint32
	BootTimeout     time.Duration

	// StoragePath, if set, makes /var/lib/docker (and anything else backed
	// by the session's storage) persist across VM restarts in a real VHD at
	// this Windows path. Leave empty for ephemeral tmpfs-backed storage,
	// wiped whenever the VM restarts.
	StoragePath      string
	MaxStorageSizeMB uint64

	// Volumes creates additional named docker volumes as part of session
	// creation, each backed by its own separate .vhdx file (independent of
	// the main session storage above) under StoragePath/volumes/<name>.vhdx.
	// If session creation fails partway through this list, the session is
	// torn down and NewSession returns an error - volume creation is
	// all-or-nothing, not partial.
	//
	// Requires StoragePath to be set: the VHD-backed volume driver derives
	// each volume's file location from the session's own storage path, so
	// there's nowhere sensible for it to live with ephemeral tmpfs storage.
	Volumes []VolumeOptions
}

// VolumeOptions describes one additional VHD-backed docker volume to create
// alongside a Session (see Options.Volumes). It ends up as a normal docker
// named volume - usable via a container's usual volume mount options - just
// backed by a real, independent .vhdx rather than a directory under the
// main session storage.
type VolumeOptions struct {
	Name   string // required, must be unique within the session
	SizeMB uint64 // required, must be > 0
	Fixed  bool   // fixed-size vhdx if true; dynamically expanding (default) if false
}
