//go:build windows

package wslcsession

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// This file is the boundary between the raw COM ABI (com.go/types.go -
// vtable slots, C struct layouts, unsafe.Pointer, manual runtime.KeepAlive)
// and the rest of the package. Everything below implements one of these
// three interfaces, each mirroring one real COM interface from wslc.idl
// one-for-one but with Go-native parameter and return types (string,
// []string, bool) instead of *uint16/*byte/uintptr - session.go and conn.go
// only ever see these interfaces, never a raw pointer or vtable slot.

// sessionManager mirrors IWSLCSessionManager (wslc.idl), restricted to the
// one method this library uses.
type sessionManager interface {
	// CreateSession calls IWSLCSessionManager::CreateSession (vtable slot
	// slotSessionManagerCreateSession). Never sets
	// WSLCSessionFlagsPersistent - see the Session doc comment in
	// session.go for why.
	CreateSession(opts Options) (wslcSession, error)
	Release()
}

// wslcSession mirrors IWSLCSession (wslc.idl), restricted to the methods
// this library uses.
type wslcSession interface {
	GetDisplayName() (string, error)
	Terminate() error
	MountWindowsFolder(windowsPath, linuxPath string, readOnly bool) error
	CreateRootNamespaceProcess(executable string, argv []string, withStdin bool) (wslcProcess, error)
	CreateVolume(opts VolumeOptions) error
	Release()
}

// wslcProcess mirrors IWSLCProcess (wslc.idl), restricted to the method
// this library uses.
type wslcProcess interface {
	GetStdHandle(fd int32) (socketHandle, error)
	Release()
}

// socketHandle is a Winsock SOCKET handle, as returned by
// IWSLCProcess::GetStdHandle for a guest process's stdin/stdout - a real
// hvsocket-relayed connection under the hood (see conn.go). It's a named
// type rather than a bare uintptr so call sites read as "this is a handle",
// not "this is some pointer-sized number".
type socketHandle uintptr

// GuestExecError wraps a CreateRootNamespaceProcess failure together with
// the guest-side errno reported alongside it. WSLCSession.cpp initializes
// the errno to -1 before attempting the exec, specifically "to make sure
// not to return 0 if something fails" - so Errno == -1 means "no specific
// errno available", not "success".
type GuestExecError struct {
	Err   error
	Errno int32
}

func (e *GuestExecError) Error() string { return fmt.Sprintf("%v (guest errno=%d)", e.Err, e.Errno) }
func (e *GuestExecError) Unwrap() error { return e.Err }

// activateSessionManager calls CoCreateInstance for CLSID
// a9b7a1b9-0671-405c-95f1-e0612cb4ce8f (WSLCSessionManager), which lives
// inside the always-running WSLService Windows service (confirmed via its
// AppID's LocalService value). This alone doesn't launch a new process -
// that happens inside CreateSession, when the service spins up a dedicated
// wslcsession.exe for the new session via WSLCSessionFactory.
//
// It then asks the service its version, which decides both which struct
// layout CreateSession may send and which vtable slots every session call
// may use. That question comes first because the interface's IID is the same
// on every build (see types.go), so activation itself succeeds against a
// service this library cannot speak to.
func activateSessionManager() (sessionManager, error) {
	ptr, err := coCreateInstance(&clsidWSLCSessionManager, &iidIWSLCSessionManager, clsctxLocalServer)
	if err != nil {
		return nil, fmt.Errorf("wslcsession: activate WSLCSessionManager: %w", err)
	}

	// GetVersion is the one call that is safe to make before knowing which
	// build is answering: it has been method 1 with a single out parameter
	// for as long as the interface has existed, so it cannot be the call
	// that lands on the wrong slot.
	version, err := managerVersion(ptr)
	if err != nil {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: GetVersion: %w", err)
	}
	if !version.atLeast(minSupportedVersion) {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: WSL %s is too old for this build: WSL %s changed the "+
			"WSLC session ABI, and calling an older service would marshal every CreateSession as "+
			"a struct it does not have; update WSL with `wsl --update --pre-release`",
			version, minSupportedVersion)
	}
	return &comSessionManager{ptr: ptr, version: version}, nil
}

// managerVersion calls IWSLCSessionManager::GetVersion.
func managerVersion(ptr unsafe.Pointer) (wslcVersion, error) {
	var version wslcVersion
	hr := vtblCall(ptr, slotSessionManagerGetVersion, uintptr(unsafe.Pointer(&version)))
	if err := hrErr(hr); err != nil {
		return wslcVersion{}, err
	}
	return version, nil
}

// abiError explains RPC_X_BAD_STUB_DATA, which is what a call marshalled to
// the wrong layout comes back as: the service's proxy read the arguments per
// its own build of wslc.idl and found them malformed. Nothing about the call
// itself is wrong, so the bare HRESULT sends whoever reads it looking in the
// wrong place - it did exactly that when WSL 2.9.5's two new
// WSLCSessionSettings fields first reached a host here. Every other failure
// is returned unchanged.
func abiError(err error, version wslcVersion) error {
	if !isHRESULT(err, hrRPCXBadStubData) {
		return err
	}
	return fmt.Errorf("WSL %s does not lay out the WSLC COM interface the way this build expects "+
		"(derived from WSL %s); that interface is private and changes between WSL releases, so "+
		"the layouts in types.go have to be re-derived against this one: %w",
		version, derivedVersion, err)
}

// acquireVmLease is the AcquireVmLease argument WSL 2.9.5 added to
// CreateRootNamespaceProcess and MountWindowsFolder. TRUE is the ordinary
// client value: start the VM if none is running, and wait out an announced
// stop so the call is served by a fresh one rather than by a VM already
// committed to going away. It also attaches a keep-alive token to the
// returned process, which is what stops wslc's idle worker from tearing the
// VM down under a long-lived guest process - the control-plane relay being
// exactly that. FALSE exists for calls wslc's own plugins make, which must
// never extend a VM's life.
const acquireVmLease uintptr = 1

type comSessionManager struct {
	ptr     unsafe.Pointer
	version wslcVersion
}

func (m *comSessionManager) Release() { comRelease(m.ptr) }

func (m *comSessionManager) CreateSession(opts Options) (wslcSession, error) {
	displayName := opts.DisplayName
	if displayName == "" {
		displayName = fmt.Sprintf("wslcsession-%d", os.Getpid())
	}

	dnPtr, err := syscall.UTF16PtrFromString(displayName)
	if err != nil {
		return nil, err
	}

	var spPtr *uint16
	if opts.StoragePath != "" {
		spPtr, err = syscall.UTF16PtrFromString(opts.StoragePath)
		if err != nil {
			return nil, err
		}
	}

	cpuCount := opts.CPUCount
	if cpuCount == 0 {
		cpuCount = 2
	}
	memoryMB := opts.MemoryMB
	if memoryMB == 0 {
		memoryMB = 2048
	}
	bootTimeoutMs := uint32(60000)
	if opts.BootTimeout > 0 {
		bootTimeoutMs = uint32(opts.BootTimeout.Milliseconds())
	}

	// HostLoopback and IdleTimeoutSec are deliberately left zero; see the
	// fields' own comments in types.go for why neither matters here. What
	// does matter is that they exist in the struct at all, which is the
	// whole of the 2.9.5 ABI break.
	settings := wslcSessionSettings{
		DisplayName:          dnPtr,
		StoragePath:          spPtr,
		MaximumStorageSizeMb: opts.MaxStorageSizeMB,
		CPUCount:             cpuCount,
		MemoryMb:             memoryMB,
		BootTimeoutMs:        bootTimeoutMs,
		NetworkingMode:       networkingModeNAT,
	}

	var sessionPtr unsafe.Pointer
	hr := vtblCall(m.ptr, slotSessionManagerCreateSession,
		uintptr(unsafe.Pointer(&settings)),
		uintptr(sessionFlagNone),
		0, // warning callback
		uintptr(unsafe.Pointer(&sessionPtr)))
	runtime.KeepAlive(dnPtr)
	runtime.KeepAlive(spPtr)
	runtime.KeepAlive(settings)
	if err := hrErr(hr); err != nil {
		return nil, fmt.Errorf("wslcsession: CreateSession: %w", abiError(err, m.version))
	}
	return &comSession{ptr: sessionPtr, version: m.version, slots: slotsForVersion(m.version)}, nil
}

type comSession struct {
	ptr     unsafe.Pointer
	version wslcVersion
	slots   sessionSlots
}

func (s *comSession) Release() { comRelease(s.ptr) }

func (s *comSession) Terminate() error {
	return hrErr(vtblCall(s.ptr, s.slots.terminate))
}

func (s *comSession) GetDisplayName() (string, error) {
	var p *uint16
	hr := vtblCall(s.ptr, s.slots.getDisplayName, uintptr(unsafe.Pointer(&p)))
	if err := hrErr(hr); err != nil {
		return "", err
	}
	return syscall.UTF16ToString(unsafe.Slice(p, 256)), nil
}

func (s *comSession) MountWindowsFolder(windowsPath, linuxPath string, readOnly bool) error {
	wPtr, err := syscall.UTF16PtrFromString(windowsPath)
	if err != nil {
		return err
	}
	lPtr, err := syscall.BytePtrFromString(linuxPath)
	if err != nil {
		return err
	}

	var ro uintptr
	if readOnly {
		ro = 1
	}

	hr := vtblCall(s.ptr, s.slots.mountWindowsFolder,
		uintptr(unsafe.Pointer(wPtr)), uintptr(unsafe.Pointer(lPtr)), ro, acquireVmLease)
	runtime.KeepAlive(wPtr)
	runtime.KeepAlive(lPtr)
	return abiError(hrErr(hr), s.version)
}

func (s *comSession) CreateRootNamespaceProcess(executable string, argv []string, withStdin bool) (wslcProcess, error) {
	exePtr, err := syscall.BytePtrFromString(executable)
	if err != nil {
		return nil, err
	}

	cmdLine, keepAlive, err := makeStringArray(argv)
	if err != nil {
		return nil, err
	}
	env, envKeepAlive, err := makeStringArray(nil)
	if err != nil {
		return nil, err
	}

	flags := processFlagNone
	if withStdin {
		flags = processFlagStdin
	}

	options := wslcProcessOptions{
		CommandLine: cmdLine,
		Environment: env,
		Flags:       flags,
	}

	var processPtr unsafe.Pointer
	var errNo int32
	hr := vtblCall(s.ptr, s.slots.createRootNamespaceProcess,
		uintptr(unsafe.Pointer(exePtr)),
		uintptr(unsafe.Pointer(&options)),
		0, 0, // ttyRows, ttyColumns - unused, no Tty flag set
		acquireVmLease,
		uintptr(unsafe.Pointer(&processPtr)),
		uintptr(unsafe.Pointer(&errNo)))
	runtime.KeepAlive(exePtr)
	runtime.KeepAlive(keepAlive)
	runtime.KeepAlive(envKeepAlive)
	runtime.KeepAlive(options)
	if err := hrErr(hr); err != nil {
		return nil, &GuestExecError{Err: abiError(err, s.version), Errno: errNo}
	}
	return &comProcess{ptr: processPtr}, nil
}

// CreateVolume calls IWSLCSession::CreateVolume with the "vhd" driver (see
// WSLCVhdVolume.cpp / WSLCVolumeMetadata.h) to create a named docker volume
// backed by its own .vhdx, independent of the session's main storage.
func (s *comSession) CreateVolume(v VolumeOptions) error {
	if v.Name == "" {
		return fmt.Errorf("wslcsession: volume name is required")
	}
	if v.SizeMB == 0 {
		return fmt.Errorf("wslcsession: volume %q: SizeMB must be > 0", v.Name)
	}

	namePtr, err := syscall.BytePtrFromString(v.Name)
	if err != nil {
		return err
	}
	driverPtr, err := syscall.BytePtrFromString(volumeDriverVhd)
	if err != nil {
		return err
	}

	sizeBytesKeyPtr, _ := syscall.BytePtrFromString(driverOptSizeBytes)
	sizeBytesValPtr, _ := syscall.BytePtrFromString(strconv.FormatUint(v.SizeMB*1024*1024, 10))
	fixedKeyPtr, _ := syscall.BytePtrFromString(driverOptFixed)
	fixedValPtr, _ := syscall.BytePtrFromString(strconv.FormatBool(v.Fixed))

	driverOpts := []wslcKeyValuePair{
		{Key: sizeBytesKeyPtr, Value: sizeBytesValPtr},
		{Key: fixedKeyPtr, Value: fixedValPtr},
	}

	options := wslcVolumeOptions{
		Name:            namePtr,
		Driver:          driverPtr,
		DriverOpts:      &driverOpts[0],
		DriverOptsCount: uint32(len(driverOpts)),
	}

	var info wslcVolumeInformation
	hr := vtblCall(s.ptr, s.slots.createVolume,
		uintptr(unsafe.Pointer(&options)),
		uintptr(unsafe.Pointer(&info)))
	runtime.KeepAlive(namePtr)
	runtime.KeepAlive(driverPtr)
	runtime.KeepAlive(driverOpts)
	runtime.KeepAlive(sizeBytesKeyPtr)
	runtime.KeepAlive(sizeBytesValPtr)
	runtime.KeepAlive(fixedKeyPtr)
	runtime.KeepAlive(fixedValPtr)
	return abiError(hrErr(hr), s.version)
}

type comProcess struct {
	ptr unsafe.Pointer
}

func (p *comProcess) Release() { comRelease(p.ptr) }

func (p *comProcess) GetStdHandle(fd int32) (socketHandle, error) {
	var h wslcHandle
	hr := vtblCall(p.ptr, slotProcessGetStdHandle, uintptr(fd), uintptr(unsafe.Pointer(&h)))
	if err := hrErr(hr); err != nil {
		return 0, err
	}
	// Observed to always be Socket in practice, but WSLCHandle is a genuine
	// discriminated union - fail clearly here rather than silently handing
	// a File or Pipe handle to raw Winsock recv()/send() later, which would
	// misbehave instead of erroring.
	if h.Type != handleTypeSocket {
		return 0, fmt.Errorf("wslcsession: expected a Socket handle, got %s", h.Type)
	}
	return socketHandle(h.Handle), nil
}

// makeStringArray builds a wslcStringArray (an array of char*) from a Go
// string slice. The returned keepAlive value must stay reachable for as
// long as the resulting wslcStringArray is passed to a syscall - it holds
// the actual *byte pointers (and the slice backing them) so Go's GC doesn't
// collect them out from under the in-flight call.
func makeStringArray(items []string) (wslcStringArray, any, error) {
	if len(items) == 0 {
		return wslcStringArray{}, nil, nil
	}

	ptrs := make([]*byte, len(items))
	for i, s := range items {
		p, err := syscall.BytePtrFromString(s)
		if err != nil {
			return wslcStringArray{}, nil, err
		}
		ptrs[i] = p
	}

	return wslcStringArray{
		Values: (*uintptr)(unsafe.Pointer(&ptrs[0])),
		Count:  uint32(len(ptrs)),
	}, ptrs, nil
}
