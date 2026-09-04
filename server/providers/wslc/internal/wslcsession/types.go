//go:build windows

package wslcsession

import "fmt"

// Field layouts below are verified against the WSL service's own IDL and the
// MIDL-generated headers (via `midl` run against src/windows/service/inc in
// the WSL repo), not guessed. Go's struct layout follows the same
// natural-alignment rules as the C compiler that generated those headers, so
// field order + type alone (no manual padding) reproduces an identical byte
// layout.
//
// Two interfaces are mirrored here, and which one a call belongs on is the
// most important thing in this file.
//
// WSLCCompat.idl is the SDK-facing interface - what wslcsdk.dll (the
// Microsoft.WSL.Containers package) talks to, reached by activating the same
// CLSID with a different IID, exactly as wslcsdk.cpp's CreateSessionManager
// does. Its header says "Changes in this file must maintain backwards
// compatibility", it carries a version handshake in
// IsClientVersionSupported, and across WSL 2.9.0 to 2.9.10 it changed by one
// appended method while the private interface below broke twice. Everything
// this library can call there, it calls there.
//
// wslc.idl is the service's private interface, whose own header says "ABI
// breaking changes in this file are OK, since both client & server always
// ship together. The WSLC SDK must not use this file". Two methods this
// library needs have no SDK-facing equivalent - MountWindowsFolder ("used
// only for testing (and by the plugin API)") and CreateRootNamespaceProcess
// ("meant for debugging") - and those two calls, alone, are made on it. The
// same session object implements both interfaces, so one QueryInterface
// reaches them.
//
// What that division buys: no struct this library sends is defined by the
// private interface any more, which is what broke on WSL 2.9.10 (2.9.5 had
// added two fields to WSLCSessionSettings; the SDK-facing copy of that
// struct was left alone, and still is). What it does not buy: the two
// private methods still sit at vtable slots that move - 2.9.10 inserted
// GetEvents as method 6 and pushed both of them down one - so their slots
// are chosen from the version the service reports.

var (
	clsidWSLCSessionManager = mustGUID("a9b7a1b9-0671-405c-95f1-e0612cb4ce8f")

	// WSLCCompat.idl - the SDK-facing interfaces.
	iidIWSLCCompatSessionManager = mustGUID("279F2047-DA35-45A3-B671-AFD2302D5D16")
	iidIWSLCCompatProcess        = mustGUID("AC69DD0D-6616-4C4F-91F2-C39D6034EA82")

	// wslc.idl - the private session interface, reached by QueryInterface on
	// the session the SDK-facing CreateSession returns, and used for nothing
	// but MountWindowsFolder and CreateRootNamespaceProcess.
	iidIWSLCSession = mustGUID("EF0661E4-6364-40EA-B433-E2FDF11F3519")
)

// Absolute vtable slot indices (0-based, counting IUnknown's
// QueryInterface=0/AddRef=1/Release=2), derived as 3 + (IDL method number -
// 1). Unlike the C# prototype this library grew out of, Go's vtblCall takes
// the slot directly, so there's no need for "placeholder" method
// declarations to keep slot numbers aligned - just the right number here.
//
// These are constants because they are the SDK-facing interface's, and it
// promises not to move them. IWSLCCompatSession's numbering is the one that
// includes OpenContainer, added as method 8 in WSL 2.9.5 - the same release
// the service raised its minimum supported client version to, and the reason
// minSupportedVersion below is what it is.
const (
	slotCompatManagerGetVersion               = 3 // IWSLCCompatSessionManager method 1
	slotCompatManagerIsClientVersionSupported = 4 // method 2
	slotCompatManagerCreateSession            = 5 // method 3

	slotCompatSessionTerminate    = 11 // IWSLCCompatSession method 9
	slotCompatSessionCreateVolume = 14 // method 12

	slotCompatProcessGetStdHandle = 5 // IWSLCCompatProcess method 3
)

// sessionSlots holds the two private IWSLCSession slots this library still
// calls, because the SDK-facing interface exposes no equivalent. They are
// the one part of the vtable that moves: WSL 2.9.10 inserted GetEvents as
// method 6, so on an older build both of these sit one slot lower. A call
// made at the wrong slot is a call to a different method with this one's
// arguments - MountWindowsFolder landing on UnmountWindowsFolder - so the
// set is chosen from the version the service reports rather than assumed.
type sessionSlots struct {
	createRootNamespaceProcess int
	mountWindowsFolder         int
}

// slotsForVersion returns the private IWSLCSession slots the given service
// exposes.
func slotsForVersion(v wslcVersion) sessionSlots {
	if v.atLeast(getEventsVersion) {
		// CreateRootNamespaceProcess is method 22, MountWindowsFolder 25.
		return sessionSlots{
			createRootNamespaceProcess: 24,
			mountWindowsFolder:         27,
		}
	}
	// 2.9.5 through 2.9.9, with no GetEvents ahead of them:
	// CreateRootNamespaceProcess is method 21, MountWindowsFolder 24.
	return sessionSlots{
		createRootNamespaceProcess: 23,
		mountWindowsFolder:         26,
	}
}

// wslcVersion mirrors _WSLCCompatVersion: the WSL package version the
// service reports through GetVersion, which is the installer's own 2.9.10.0
// without its trailing build number.
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

// The WSL releases this library's knowledge of both interfaces is keyed to.
//
// minSupportedVersion is 2.9.5, which is where both interfaces turn on. It
// is where IWSLCCompatSession gained OpenContainer as method 8, moving every
// slot after it - the service's own IsClientVersionSupported refuses clients
// older than this for that exact reason - and where the private methods
// gained their AcquireVmLease argument.
//
// getEventsVersion is the release that inserted IWSLCSession::GetEvents, and
// so selects between the two sets of private slots.
//
// derivedVersion is the release every layout here was read off. It is the
// client version handed to IsClientVersionSupported, and it is named in the
// error a future ABI change produces, so the message says what has to be
// re-derived and against what.
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

// compatHandle mirrors _WSLCCompatHandle: a discriminated union of a HANDLE,
// which in Go just needs the union's storage (pointer-sized) - Go's struct
// layout engine inserts the same 4 bytes of padding after Type that a C
// compiler would, to align Handle to 8 bytes.
type compatHandle struct {
	Type   wslcHandleType
	Handle uintptr
}

// compatSessionSettings mirrors _WSLCCompatSessionSettings.
//
// This is the same shape the private WSLCSessionSettings had before WSL
// 2.9.5 gave it a HostLoopback pointer and an IdleTimeoutSec, and it has not
// moved since - the clearest evidence available of what the SDK-facing
// interface's compatibility promise is worth. The service zero-initializes
// the private struct it converts this into, so both of those fields end up
// exactly as this library used to set them itself: a null HostLoopback (no
// host-loopback DNS record in the guest, which is right - a guest here
// reaches the host over the relay's stdio, never over the network) and an
// IdleTimeoutSec of 0 (wslc's own 30s grace period before it tears an idle
// VM down; a session holding the relay's process reference is never idle,
// see acquireVmLease in comapi.go).
type compatSessionSettings struct {
	DisplayName          *uint16
	StoragePath          *uint16
	MaximumStorageSizeMb uint64
	CPUCount             uint32
	MemoryMb             uint32
	BootTimeoutMs        uint32
	NetworkingMode       int32
	FeatureFlags         int32
	DmesgOutput          compatHandle
	StorageFlags         int32

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
// (char*) plus a count. It is spelled from the private IDL because the
// options struct it belongs to is the root-namespace exec's, which the
// SDK-facing interface has no equivalent of.
type wslcStringArray struct {
	Values *uintptr
	Count  uint32
}

// wslcProcessOptions mirrors _WSLCProcessOptions (wslc.idl). Private for the
// same reason as wslcStringArray - and unchanged across every release this
// library supports.
type wslcProcessOptions struct {
	CurrentDirectory *byte
	User             *byte
	CommandLine      wslcStringArray
	Environment      wslcStringArray
	Flags            int32
}

// compatKeyValuePair mirrors _WSLCCompatKeyValuePair, aliased as both
// WSLCCompatDriverOption and WSLCCompatLabel.
type compatKeyValuePair struct {
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

// compatVolumeOptions mirrors _WSLCCompatVolumeOptions.
type compatVolumeOptions struct {
	Name            *byte
	Driver          *byte
	DriverOpts      *compatKeyValuePair
	DriverOptsCount uint32
	Labels          *compatKeyValuePair
	LabelsCount     uint32
}

// compatVolumeInformation mirrors _WSLCCompatVolumeInformation: two
// fixed-size char arrays, each WSLCCompat_MAX_*_LENGTH + 1 bytes.
type compatVolumeInformation struct {
	Name   [256]byte
	Driver [256]byte
}
