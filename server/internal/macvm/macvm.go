// Package macvm boots a macOS guest under Virtualization.framework.
//
// It is a proof of concept, and it is deliberately not wired into the vz
// provider: a macOS guest cannot host the pool's Docker daemon, because
// Virtualization.framework offers nested virtualization only through
// VZGenericPlatformConfiguration — the Linux platform — so nothing a pool does
// today would work inside one. What this package answers is the question
// underneath that: what it takes to install and boot macOS the way internal/vzvm
// installs and boots Linux.
//
// The split follows vzvm's, and for the same reason: everything that can be
// expressed without the framework lives here so the command and its tests build
// on every platform, the cgo is confined to the darwin && cgo file, and a stub
// covers the rest.
//
// A macOS guest replaces vzvm's kernel, initrd, and command line with four
// files that must be created together and then kept together for the life of
// the guest:
//
//   - a hardware model, chosen by the restore image and fixed at install;
//   - a machine identifier, unique per guest — two VMs sharing one is undefined
//     behavior in the guest;
//   - an auxiliary storage (the guest's NVRAM), laid out for that hardware model;
//   - the disk the installer formats.
//
// They are a set. Reinstalling means creating all four again, and moving a guest
// means moving all four.
package macvm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/adrg/xdg"
)

// ErrUnsupported reports that this build cannot run Virtualization.framework.
// What is wrong differs by build, so the stub wraps it with the reason.
var ErrUnsupported = errors.New("macvm: this build cannot run Apple Virtualization.framework")

// Bundle is one macOS guest on disk: the four files that constitute it, plus
// what the install chose.
type Bundle struct {
	// Dir holds every file of this guest and nothing else.
	Dir string
}

const (
	diskName              = "disk.img"
	auxiliaryName         = "aux.img"
	hardwareModelName     = "hardwaremodel"
	machineIdentifierName = "machineidentifier"
	metadataName          = "bundle.json"
	stateName             = "state.bin"
	macAddressName        = "macaddress"
)

// DefaultRoot is where bundles live, beside the vz provider's own state: on
// macOS ~/Library/Application Support/discobox/macvm.
func DefaultRoot() string {
	if home := strings.TrimSpace(xdg.DataHome); home != "" {
		return filepath.Join(home, "discobox", "macvm")
	}
	return filepath.Join(os.TempDir(), "discobox", "macvm")
}

// OpenBundle names a guest under root. The name becomes a directory name, so it
// is checked here rather than at filepath.Join.
func OpenBundle(root, name string) (Bundle, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = DefaultRoot()
	}
	if err := validateName(name); err != nil {
		return Bundle{}, err
	}
	return Bundle{Dir: filepath.Join(root, name)}, nil
}

func validateName(name string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return errors.New("macvm: guest name is required")
	case name == "." || name == "..":
		return fmt.Errorf("macvm: guest name %q is reserved", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("macvm: guest name %q may not contain a path separator", name)
	}
	return nil
}

// DiskPath is the raw disk image the installer formats and the guest boots.
func (b Bundle) DiskPath() string { return filepath.Join(b.Dir, diskName) }

// AuxiliaryPath is the guest's NVRAM, laid out for its hardware model.
func (b Bundle) AuxiliaryPath() string { return filepath.Join(b.Dir, auxiliaryName) }

// HardwareModelPath is the hardware model's opaque data representation.
func (b Bundle) HardwareModelPath() string { return filepath.Join(b.Dir, hardwareModelName) }

// MachineIdentifierPath is this guest's unique identifier, as opaque data.
func (b Bundle) MachineIdentifierPath() string {
	return filepath.Join(b.Dir, machineIdentifierName)
}

// MetadataPath records what the install chose, for a later run to default to.
func (b Bundle) MetadataPath() string { return filepath.Join(b.Dir, metadataName) }

// MACAddressPath is the guest's network address. It is part of the guest's
// identity, like the machine identifier: stable across boots, so the guest
// keeps its lease, and carried by a snapshot, whose memory holds a network
// stack configured for it. A cold clone gets a new one.
func (b Bundle) MACAddressPath() string { return filepath.Join(b.Dir, macAddressName) }

// StatePath is a saved machine state: the guest's memory and device state,
// written while it was paused. It is the third member of a snapshot, and it is
// only meaningful beside the exact disk it was taken with — the framework
// restores memory that believes the disk is where it left it.
//
// It is also host-bound: the framework encrypts it with a key tied to the Mac
// that wrote it, and a host OS update can invalidate it. A missing or rejected
// state is therefore never fatal; it means boot instead of resume.
func (b Bundle) StatePath() string { return filepath.Join(b.Dir, stateName) }

// HasState reports whether a snapshot has been taken, and how large it is.
func (b Bundle) HasState() (int64, bool) {
	info, err := os.Stat(b.StatePath())
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

// Installed reports whether every file a boot needs is present, naming the
// first one that is not. A partial bundle is what an interrupted install
// leaves, and booting it fails inside the framework with nothing to act on.
func (b Bundle) Installed() error {
	for _, path := range []string{
		b.DiskPath(),
		b.AuxiliaryPath(),
		b.HardwareModelPath(),
		b.MachineIdentifierPath(),
	} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("macvm: %s is not installed: %w", b.Dir, err)
		}
	}
	return nil
}

// Metadata is what the install chose, so a run does not have to be told again.
type Metadata struct {
	MacOSVersion string    `json:"macosVersion,omitempty"`
	BuildVersion string    `json:"buildVersion,omitempty"`
	CPUCount     uint      `json:"cpuCount,omitempty"`
	MemoryBytes  uint64    `json:"memoryBytes,omitempty"`
	DiskBytes    int64     `json:"diskBytes,omitempty"`
	InstalledAt  time.Time `json:"installedAt,omitempty"`
	// ProvisionedUser is the account a provisioning boot was started with. It
	// records the request, not the outcome: the guest reports nothing back,
	// and it evaluates provisioning only once, so a second request is ignored.
	ProvisionedUser string `json:"provisionedUser,omitempty"`
}

// MacOSMajor is the guest's major version, or 0 when the install did not
// record one.
func (m Metadata) MacOSMajor() int {
	major, _, _ := strings.Cut(m.MacOSVersion, ".")
	value := 0
	for _, r := range major {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
	}
	return value
}

// ReadMetadata returns what the install recorded. A bundle without it is not an
// error: the four files are what a boot needs, and this only carries defaults.
func (b Bundle) ReadMetadata() (Metadata, error) {
	data, err := os.ReadFile(b.MetadataPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, nil
		}
		return Metadata{}, fmt.Errorf("macvm: read %s: %w", b.MetadataPath(), err)
	}
	var metadata Metadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return Metadata{}, fmt.Errorf("macvm: parse %s: %w", b.MetadataPath(), err)
	}
	return metadata, nil
}

func (b Bundle) writeMetadata(metadata Metadata) error {
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("macvm: encode metadata: %w", err)
	}
	if err := os.WriteFile(b.MetadataPath(), append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("macvm: write %s: %w", b.MetadataPath(), err)
	}
	return nil
}

// GuestProvisioning is the account a macOS 27 or newer guest creates on its
// first boot after restore, in place of a person answering Setup Assistant.
//
// There is no uid or gid in it because the framework has none to give. It does
// not need one for the common case: the first account on a Mac is 501 in staff
// (20), which is also what the first account on the host is.
type GuestProvisioning struct {
	FullName string
	Username string
	Password string
	// AutoLogin logs the account in at startup.
	AutoLogin bool
	// RemoteLogin turns on SSH.
	RemoteLogin bool
}

func (p GuestProvisioning) validate() error {
	switch {
	case strings.TrimSpace(p.Username) == "":
		return errors.New("macvm: provisioning needs a username")
	case p.Password == "":
		return errors.New("macvm: provisioning needs a password")
	}
	return nil
}

// SharedDirectory is one host directory exported to the guest over virtiofs.
//
// An empty tag means the automount tag, which a macOS 13 or newer guest mounts
// by itself at /Volumes/My Shared Files. Any other tag has to be mounted in the
// guest by hand with mount_virtiofs, which is the whole difference from the
// Linux guest, where fstab does it.
type SharedDirectory struct {
	Tag      string
	HostPath string
	ReadOnly bool
}

// InstallOptions describes one installation. The restore image is a local file:
// the framework installs from an IPSW on disk, never from a URL.
type InstallOptions struct {
	Bundle           Bundle
	RestoreImagePath string
	// DiskBytes is the raw disk the installer formats. It is sparse, so it
	// costs what the guest writes, and it cannot be grown after install
	// without resizing the guest's APFS container from inside the guest.
	DiskBytes int64
	// Sparse creates the disk as an ASIF image (macOS 26 and newer) instead of
	// a raw one. Both are sparse on APFS and both clone, so this buys Apple's
	// recommended format for VM storage rather than a different capability.
	Sparse bool
	// CPUCount and MemoryBytes are raised to the restore image's minimums when
	// they are below them, and clamped to what the framework accepts.
	CPUCount    uint
	MemoryBytes uint64
	// Progress is called with the installer's fraction, if set.
	Progress func(fraction float64)
}

// Validate checks what can be checked without a hypervisor.
func (o InstallOptions) Validate() error {
	if strings.TrimSpace(o.Bundle.Dir) == "" {
		return errors.New("macvm: bundle directory is required")
	}
	if strings.TrimSpace(o.RestoreImagePath) == "" {
		return errors.New("macvm: restore image path is required")
	}
	if o.DiskBytes <= 0 {
		return errors.New("macvm: disk size must be greater than zero")
	}
	return nil
}

// RunOptions describes one boot of an installed bundle.
type RunOptions struct {
	Bundle Bundle
	// CPUCount and MemoryBytes default to what the install recorded.
	CPUCount    uint
	MemoryBytes uint64
	// GUI opens the framework's own window on the guest's framebuffer. It takes
	// over the calling thread, which must be the main thread and must be locked
	// to its goroutine, and it returns when the window closes or the guest
	// stops.
	GUI                       bool
	WindowWidth, WindowHeight float64
	// Display is the guest's framebuffer. Its pixels-per-inch is what decides
	// whether the guest treats it as a Retina display.
	DisplayWidth, DisplayHeight, DisplayPPI int64
	// Recovery boots into macOS Recovery instead of the installed system.
	Recovery bool
	// Restore resumes from the bundle's saved state instead of booting. The
	// configuration has to match the one that was saved — memory included; a
	// larger size is rejected, not grown into — which is why a restore takes
	// its size from the bundle's metadata rather than from a flag.
	//
	// A restore also needs the console user's session unlocked: the framework
	// keeps the key that protects saved state behind the login session, so on
	// a locked Mac a restore fails with "permission denied" while a save still
	// succeeds.
	Restore bool
	// SaveOnExit pauses the guest and writes its state before stopping, instead
	// of asking it to shut down. What it leaves behind is only valid against
	// the disk as it stands at that moment, so the pair is a snapshot and the
	// way to use one twice is to clone it.
	SaveOnExit bool
	// Provision creates an account on the guest's first boot after restore.
	// Only a macOS 27 or newer guest honors it, and only on that first boot.
	Provision *GuestProvisioning
	Shares    []SharedDirectory
}

// Validate checks what can be checked without a hypervisor.
func (o RunOptions) Validate() error {
	if strings.TrimSpace(o.Bundle.Dir) == "" {
		return errors.New("macvm: bundle directory is required")
	}
	if err := o.Bundle.Installed(); err != nil {
		return err
	}
	if o.GUI && (o.WindowWidth <= 0 || o.WindowHeight <= 0) {
		return errors.New("macvm: a GUI window needs a width and a height")
	}
	if o.DisplayWidth <= 0 || o.DisplayHeight <= 0 || o.DisplayPPI <= 0 {
		return errors.New("macvm: display width, height, and pixels-per-inch are required")
	}
	if o.Provision != nil {
		if err := o.Provision.validate(); err != nil {
			return err
		}
		if o.Restore {
			return errors.New("macvm: a restored guest has already booted, so it cannot be provisioned")
		}
	}
	if o.Restore {
		if _, ok := o.Bundle.HasState(); !ok {
			return fmt.Errorf("macvm: %s has no saved state to restore", o.Bundle.Dir)
		}
		if o.Recovery {
			return errors.New("macvm: a restore resumes a running guest, so it cannot boot into recovery")
		}
		if o.CPUCount == 0 || o.MemoryBytes == 0 {
			return errors.New("macvm: a restore needs the size the state was saved with; the bundle's metadata is missing it")
		}
	}
	tags := map[string]struct{}{}
	for _, share := range o.Shares {
		if host := strings.TrimSpace(share.HostPath); host == "" || !filepath.IsAbs(host) {
			return fmt.Errorf("macvm: shared directory needs an absolute host path, got %q", share.HostPath)
		}
		tag := strings.TrimSpace(share.Tag)
		if len(tag) > maxSharedDirectoryTagLen {
			return fmt.Errorf("macvm: shared directory tag %q exceeds %d bytes", tag, maxSharedDirectoryTagLen)
		}
		if _, ok := tags[tag]; ok {
			return fmt.Errorf("macvm: shared directory tag %q is attached twice", tag)
		}
		tags[tag] = struct{}{}
	}
	return nil
}

// CloneOptions describes one copy-on-write clone. On APFS the disks cost
// nothing to copy and nothing to keep until the clone writes, so a guest is
// cheap to hand out and expensive only where it diverges.
type CloneOptions struct {
	Source, Dest Bundle
	// Snapshot carries the source's saved state into the clone, which means the
	// clone must keep the source's machine identifier: the saved memory is that
	// machine's. Two guests with one identifier is undefined behavior in the
	// guest, so a snapshot clone is one the source is not expected to run
	// again. Without it the clone gets a fresh identifier and cold boots.
	Snapshot bool
}

// Validate checks what can be checked without touching the filesystem twice.
func (o CloneOptions) Validate() error {
	if err := o.Source.Installed(); err != nil {
		return err
	}
	if strings.TrimSpace(o.Dest.Dir) == "" {
		return errors.New("macvm: clone destination is required")
	}
	if o.Dest.Dir == o.Source.Dir {
		return fmt.Errorf("macvm: %s cannot be cloned onto itself", o.Source.Dir)
	}
	if o.Snapshot {
		if _, ok := o.Source.HasState(); !ok {
			return fmt.Errorf("macvm: %s has no saved state to clone", o.Source.Dir)
		}
	}
	return nil
}

// maxSharedDirectoryTagLen is Virtualization.framework's limit on a virtiofs
// tag, checked here so an over-long one is a configuration error rather than an
// opaque framework rejection at boot.
const maxSharedDirectoryTagLen = 36
