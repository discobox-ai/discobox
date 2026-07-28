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
// library depends on an interface Microsoft can change without notice. It
// works today; a future WSL update could require re-deriving vtable slots
// and struct layouts against the wslc.idl of that version.

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
// slotSessionManagerCreateSession is method 2; method 1 (GetVersion) isn't
// used by this library and so isn't declared.
const (
	slotSessionManagerCreateSession = 4 // IWSLCSessionManager method 2

	slotSessionGetID                      = 3  // IWSLCSession method 1
	slotSessionGetDisplayName             = 4  // method 2
	slotSessionGetState                   = 5  // method 3
	slotSessionCreateRootNamespaceProcess = 23 // method 21
	slotSessionTerminate                  = 25 // method 23
	slotSessionMountWindowsFolder         = 26 // method 24
	slotSessionCreateVolume               = 32 // method 30
	slotSessionDeleteVolume               = 33 // method 31

	slotProcessSignal       = 3 // IWSLCProcess method 1
	slotProcessGetExitEvent = 4 // method 2
	slotProcessGetStdHandle = 5 // method 3
	slotProcessGetPid       = 7 // method 5
	slotProcessGetState     = 8 // method 6
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

// wslcSessionSettings mirrors _WSLCSessionSettings (wslc.idl:453-468).
type wslcSessionSettings struct {
	DisplayName          *uint16
	StoragePath          *uint16
	MaximumStorageSizeMb uint64
	CPUCount             uint32
	MemoryMb             uint32
	BootTimeoutMs        uint32
	NetworkingMode       int32
	FeatureFlags         int32
	DmesgOutput          wslcHandle
	StorageFlags         int32
	RootVhdOverride      *uint16
	RootVhdTypeOverride  *byte
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
