//go:build windows

package wslcsession

import "fmt"

// Field layouts below are verified against wslc.idl and the MIDL-generated
// wslc.h (via `midl` run against src/windows/service/inc/wslc.idl in the WSL
// repo), not guessed. Go's struct layout follows the same natural-alignment
// rules as the C compiler that generated wslc.h, so field order + type alone
// (no manual padding) reproduces an identical byte layout.
//
// This uses the internal wslc.idl interfaces (IWSLCSessionManager/
// IWSLCSession), not the documented WSLCCompat.idl ones - wslc.idl says
// "ABI breaking changes in this file are OK, since both client & server
// always ship together. The WSLC SDK must not use this file" - meaning this
// library depends on an interface Microsoft can change without notice.
//
// It already has, twice, which is why the version below is checked rather
// than assumed. WSL 2.9.5 added two fields to WSLCSessionSettings and an
// AcquireVmLease argument to two methods; WSL 2.9.10 inserted
// IWSLCSession::GetEvents as method 6 and pushed every later method down one
// vtable slot. Everything here is derived from the wslc.idl of WSL 2.9.10,
// and the IID never changed across any of it - the interface a build answers
// with is whatever that build compiled, so activation cannot tell the
// difference and a mismatched call is simply marshalled wrong.

// iidIWSLCSession isn't needed: CoCreateInstance is only ever called for
// IWSLCSessionManager (see NewSession); the resulting IWSLCSession pointer
// comes back directly from CreateSession's [out] parameter, with no
// separate QueryInterface step that would need its IID.
var (
	clsidWSLCSessionManager = mustGUID("a9b7a1b9-0671-405c-95f1-e0612cb4ce8f")
	iidIWSLCSessionManager  = mustGUID("82A7ABC8-6B50-43FC-AB96-15FBBE7E8760")
)

// Absolute vtable slot indices (0-based, counting IUnknown's
// QueryInterface=0/AddRef=1/Release=2), derived as 3 + (IDL method number -
// 1). Unlike the C# prototype this library grew out of, Go's vtblCall takes
// the slot directly, so there's no need for "placeholder" method
// declarations to keep slot numbers aligned - just the right number here.
//
// Only IWSLCSessionManager's and IWSLCProcess's slots are constants; the
// session's move between releases and live in sessionSlots below.
const (
	slotSessionManagerGetVersion    = 3 // IWSLCSessionManager method 1
	slotSessionManagerCreateSession = 4 // method 2

	slotProcessSignal       = 3 // IWSLCProcess method 1
	slotProcessGetExitEvent = 4 // method 2
	slotProcessGetStdHandle = 5 // method 3
	slotProcessGetPid       = 7 // method 5
	slotProcessGetState     = 8 // method 6
)

// sessionSlots holds the IWSLCSession slots this library calls. They are the
// one part of the vtable that moves: WSL 2.9.10 inserted GetEvents as method
// 6, so on an older build every method after it sits one slot lower. A call
// made at the wrong slot is a call to a different method with this one's
// arguments - Terminate() landing on FormatVirtualDisk, say - so the set is
// chosen from the version the service reports rather than assumed.
//
// Only the methods this library calls are tracked. The others were two more
// numbers to keep right for no caller.
type sessionSlots struct {
	getDisplayName             int
	createRootNamespaceProcess int
	terminate                  int
	mountWindowsFolder         int
	createVolume               int
}

// slotsForVersion returns the IWSLCSession slots the given service exposes.
func slotsForVersion(v wslcVersion) sessionSlots {
	if v.atLeast(getEventsVersion) {
		// GetDisplayName is method 2, CreateRootNamespaceProcess 22,
		// Terminate 24, MountWindowsFolder 25, CreateVolume 31.
		return sessionSlots{
			getDisplayName:             4,
			createRootNamespaceProcess: 24,
			terminate:                  26,
			mountWindowsFolder:         27,
			createVolume:               33,
		}
	}
	// 2.9.5 through 2.9.9, with no GetEvents ahead of them:
	// CreateRootNamespaceProcess is method 21, Terminate 23,
	// MountWindowsFolder 24, CreateVolume 30.
	return sessionSlots{
		getDisplayName:             4,
		createRootNamespaceProcess: 23,
		terminate:                  25,
		mountWindowsFolder:         26,
		createVolume:               32,
	}
}

// wslcVersion mirrors _WSLCVersion (wslc.idl): the WSL package version the
// service reports through IWSLCSessionManager::GetVersion, which is the
// installer's own 2.9.10.0 without its trailing build number.
type wslcVersion struct {
	Major    uint32
	Minor    uint32
	Revision uint32
}

func (v wslcVersion) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Revision)
}

// atLeast reports whether v is o or newer.
func (v wslcVersion) atLeast(o wslcVersion) bool {
	if v.Major != o.Major {
		return v.Major > o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor > o.Minor
	}
	return v.Revision >= o.Revision
}

// The WSL releases this library's knowledge of the interface is keyed to.
//
// minSupportedVersion is the release that gave WSLCSessionSettings the
// layout below. A service older than it reads the struct this library sends
// as the shorter one it knows, takes the wrong bytes for its pointer fields,
// and fails the call with RPC_X_BAD_STUB_DATA - which is exactly how the
// mirror image of that mismatch turned up here, as an 0x800706F7 out of
// CreateSession the day a host updated to 2.9.10. Refusing an older build up
// front is the difference between an explanation and that HRESULT.
//
// getEventsVersion is the release that inserted IWSLCSession::GetEvents, and
// so selects between the two sets of session slots.
//
// derivedVersion is the release every layout here was read off. It is named
// in the error a future ABI change produces, so the message says what has to
// be re-derived and against what.
var (
	minSupportedVersion = wslcVersion{Major: 2, Minor: 9, Revision: 5}
	getEventsVersion    = wslcVersion{Major: 2, Minor: 9, Revision: 10}
	derivedVersion      = wslcVersion{Major: 2, Minor: 9, Revision: 10}
)

// WSLCNetworkingMode.
const (
	networkingModeNone     int32 = 0
	networkingModeNAT      int32 = 1
	networkingModeConsomme int32 = 2
)

// WSLCSessionFlags. Deliberately never setting sessionFlagPersistent: per
// WSLCSessionManager.cpp's own header comment, a non-persistent session is
// torn down "when all client refs are released" - including when this
// process exits or crashes, since COM releases all of a dead process's
// outstanding references automatically. That's exactly the "dies with the
// program" lifecycle this library wants.
const (
	sessionFlagNone         uint32 = 0
	sessionFlagPersistent   uint32 = 1
	sessionFlagOpenExisting uint32 = 2
)

type wslcHandleType int32

const (
	handleTypeUnknown wslcHandleType = 0
	handleTypeFile    wslcHandleType = 1
	handleTypePipe    wslcHandleType = 2
	handleTypeSocket  wslcHandleType = 3
)

// String supports the type check in getStdHandle: GetStdHandle is only ever
// observed to return WSLCHandleTypeSocket in practice, but the field is a
// real discriminated union over three other possibilities, and silently
// treating a File or Pipe handle as a Socket (i.e. handing it to raw
// Winsock recv()/send()) would misbehave rather than fail cleanly.
func (t wslcHandleType) String() string {
	switch t {
	case handleTypeUnknown:
		return "Unknown"
	case handleTypeFile:
		return "File"
	case handleTypePipe:
		return "Pipe"
	case handleTypeSocket:
		return "Socket"
	default:
		return fmt.Sprintf("wslcHandleType(%d)", int32(t))
	}
}

// wslcHandle mirrors _WSLCHandle: a discriminated union of a HANDLE, which
// in Go just needs the union's storage (pointer-sized) - Go's struct layout
// engine inserts the same 4 bytes of padding after Type that a C compiler
// would, to align Handle to 8 bytes.
type wslcHandle struct {
	Type   wslcHandleType
	Handle uintptr
}

// wslcSessionSettings mirrors _WSLCSessionSettings as of WSL 2.9.10.
// HostLoopback and IdleTimeoutSec are the fields 2.9.5 added, and sending
// the struct without them is what made CreateSession fail with
// RPC_X_BAD_STUB_DATA on an updated host: the service's proxy walked this
// buffer expecting a pointer where the old layout had DmesgOutput.
type wslcSessionSettings struct {
	DisplayName          *uint16
	StoragePath          *uint16
	MaximumStorageSizeMb uint64
	CPUCount             uint32
	MemoryMb             uint32
	BootTimeoutMs        uint32
	NetworkingMode       int32
	FeatureFlags         int32

	// HostLoopback is the DNS name the guest resolves to the host's
	// loopback address (wslc's own CLI defaults it to
	// "host.wslc.internal"). NULL leaves the record uncreated, which is
	// what this library wants: a guest here reaches the host over the
	// relay's stdio, never over the network.
	HostLoopback *byte

	DmesgOutput  wslcHandle
	StorageFlags int32

	// IdleTimeoutSec is how long wslc lets an idle VM run before tearing it
	// down, 0 selecting its own 30s grace period. Idle means an activity
	// refcount of zero, and a client-held IWSLCProcess reference is one of
	// the things that counts - so a session with the control-plane relay
	// running is never idle and this value never comes into play. See
	// acquireVmLease in comapi.go, which is what attaches that reference.
	IdleTimeoutSec uint32

	// Below options are used for debugging purposes only.
	RootVhdOverride     *uint16
	RootVhdTypeOverride *byte
}

// WSLCFD.
const (
	fdStdin  int32 = 0
	fdStdout int32 = 1
)

// WSLCProcessFlags.
const (
	processFlagNone  int32 = 0
	processFlagStdin int32 = 1
)

// wslcStringArray mirrors _WSLCStringArray: a pointer to an array of LPCSTR
// (char*) plus a count.
type wslcStringArray struct {
	Values *uintptr
	Count  uint32
}

// wslcProcessOptions mirrors _WSLCProcessOptions (wslc.idl:172-179).
type wslcProcessOptions struct {
	CurrentDirectory *byte
	User             *byte
	CommandLine      wslcStringArray
	Environment      wslcStringArray
	Flags            int32
}

// wslcKeyValuePair mirrors KeyValuePair (wslc.idl:690-694), aliased as both
// WSLCDriverOption and WSLCLabel.
type wslcKeyValuePair struct {
	Key   *byte
	Value *byte
}

// "vhd" is the volume driver CreateVolume uses to back a named docker volume
// with its own real .vhdx file, independent of the session's main storage
// VHD (see WSLCVolumeMetadata.h / WSLCVhdVolume.cpp - WSLCVhdVolumeDriver).
const volumeDriverVhd = "vhd"

// VhdVolumeOptions.Parse (WSLCVhdVolume.cpp) reads these exact DriverOpts
// keys: SizeBytes (required, must be > 0), Fixed (optional bool), Uid/Gid
// (optional, not exposed here).
const (
	driverOptSizeBytes = "SizeBytes"
	driverOptFixed     = "Fixed"
)

// wslcVolumeOptions mirrors _WSLCVolumeOptions (wslc.idl:1696-1704).
type wslcVolumeOptions struct {
	Name            *byte
	Driver          *byte
	DriverOpts      *wslcKeyValuePair
	DriverOptsCount uint32
	Labels          *wslcKeyValuePair
	LabelsCount     uint32
}

// wslcVolumeInformation mirrors _WSLCVolumeInformation (wslc.idl:1706-1712).
type wslcVolumeInformation struct {
	Name   [256]byte
	Driver [256]byte
}
