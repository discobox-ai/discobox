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
// three interfaces, mirroring the real COM methods one-for-one but with
// Go-native parameter and return types (string, []string, bool) instead of
// *uint16/*byte/uintptr - session.go and conn.go only ever see these
// interfaces, never a raw pointer or vtable slot.
//
// Each Go interface spans two COM ones. The SDK-facing WSLCCompat.idl
// carries every call it has an equivalent for; the service's private
// wslc.idl carries the two it does not. types.go explains why that division
// is where it is; the comments below say which side each method is on.

// sessionManager mirrors IWSLCCompatSessionManager, restricted to the one
// method this library uses beyond the version handshake in
// activateSessionManager.
type sessionManager interface {
	// CreateSession calls IWSLCCompatSessionManager::CreateSession. Never
	// sets WSLCSessionFlagsPersistent - see the Session doc comment in
	// session.go for why.
	CreateSession(opts Options) (wslcSession, error)
	Release()
}

// wslcSession is one session object, addressed through both of the
// interfaces it implements: Terminate and CreateVolume are
// IWSLCCompatSession's, MountWindowsFolder and CreateRootNamespaceProcess
// are IWSLCSession's, which has no SDK-facing counterpart for either.
type wslcSession interface {
	Terminate() error
	MountWindowsFolder(windowsPath, linuxPath string, readOnly bool) error
	CreateRootNamespaceProcess(executable string, argv []string, withStdin bool) (wslcProcess, error)
	CreateVolume(opts VolumeOptions) error
	Release()
}

// wslcProcess mirrors IWSLCCompatProcess, restricted to the method this
// library uses. The process object comes back from the private
// CreateRootNamespaceProcess, but nothing private is ever called on it.
type wslcProcess interface {
	GetStdHandle(fd int32) (socketHandle, error)
	Release()
}

// socketHandle is a Winsock SOCKET handle, as returned by GetStdHandle for a
// guest process's stdin/stdout - a real hvsocket-relayed connection under
// the hood (see conn.go). It's a named type rather than a bare uintptr so
// call sites read as "this is a handle", not "this is some pointer-sized
// number".
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
// AppID's LocalService value), asking for the SDK-facing
// IWSLCCompatSessionManager - the same activation wslcsdk.dll performs. This
// alone doesn't launch a new process; that happens inside CreateSession,
// when the service spins up a dedicated wslcsession.exe for the new session
// via WSLCSessionFactory.
//
// It then agrees a version with the service, twice over, before any real
// call is made. Both questions come first because the interfaces' IIDs are
// the same on every build (see types.go), so activation itself succeeds
// against a service this library cannot speak to.
func activateSessionManager() (sessionManager, error) {
	ptr, err := coCreateInstance(&clsidWSLCSessionManager, &iidIWSLCCompatSessionManager, clsctxLocalServer)
	if err != nil {
		return nil, fmt.Errorf("wslcsession: activate WSLCSessionManager: %w", err)
	}

	// GetVersion is the one call that is safe to make before knowing which
	// build is answering: it has been method 1 with a single out parameter
	// for as long as either interface has existed, so it cannot be the call
	// that lands on the wrong slot.
	version, err := managerVersion(ptr)
	if err != nil {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: GetVersion: %w", err)
	}

	// This library's own floor, which the service cannot answer for: it
	// covers the private slots as well as the SDK-facing ones.
	if !version.atLeast(minSupportedVersion) {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: WSL %s is too old for this build: WSL %s moved both "+
			"the SDK-facing and the private WSLC interfaces, and calling an older service would "+
			"put every call at the wrong vtable slot; update WSL with `wsl --update --pre-release`",
			version, minSupportedVersion)
	}

	// The service's own floor, which only it can answer for. This is the
	// handshake the SDK performs, and the reason a future break on this
	// interface arrives as a refusal naming both versions rather than as a
	// call that is quietly marshalled wrong.
	supported, err := clientVersionSupported(ptr, derivedVersion)
	if err != nil {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: IsClientVersionSupported: %w", err)
	}
	if !supported {
		comRelease(ptr)
		return nil, fmt.Errorf("wslcsession: WSL %s no longer supports a client built against WSL "+
			"%s: the SDK-facing WSLC interface has had a breaking change, and this library has to "+
			"be re-derived against it (see types.go)", version, derivedVersion)
	}

	return &comSessionManager{ptr: ptr, version: version}, nil
}

// managerVersion calls IWSLCCompatSessionManager::GetVersion.
func managerVersion(ptr unsafe.Pointer) (wslcVersion, error) {
	var version wslcVersion
	hr := vtblCall(ptr, slotCompatManagerGetVersion, uintptr(unsafe.Pointer(&version)))
	if err := hrErr(hr); err != nil {
		return wslcVersion{}, err
	}
	return version, nil
}

// clientVersionSupported calls
// IWSLCCompatSessionManager::IsClientVersionSupported, which is how the
// service tells a client built against one release whether its idea of the
// SDK-facing interface still holds. The service raised that floor once
// already, in 2.9.5, to insert a method mid-interface.
func clientVersionSupported(ptr unsafe.Pointer, client wslcVersion) (bool, error) {
	var supported int32
	hr := vtblCall(ptr, slotCompatManagerIsClientVersionSupported,
		uintptr(unsafe.Pointer(&client)),
		uintptr(unsafe.Pointer(&supported)))
	runtime.KeepAlive(client)
	if err := hrErr(hr); err != nil {
		return false, err
	}
	return supported != 0, nil
}

// abiError explains RPC_X_BAD_STUB_DATA, which is what a call marshalled to
// the wrong layout comes back as: the service's proxy read the arguments per
// its own build of the IDL and found them malformed. Nothing about the call
// itself is wrong, so the bare HRESULT sends whoever reads it looking in the
// wrong place - it did exactly that when WSL 2.9.5's two new
// WSLCSessionSettings fields first reached a host here. Every other failure
// is returned unchanged.
func abiError(err error, version wslcVersion) error {
	if !isHRESULT(err, hrRPCXBadStubData) {
		return err
	}
	return fmt.Errorf("WSL %s does not lay out this WSLC call the way this build expects (derived "+
		"from WSL %s); the layouts in types.go have to be re-derived against this one: %w",
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

	settings := compatSessionSettings{
		DisplayName:          dnPtr,
		StoragePath:          spPtr,
		MaximumStorageSizeMb: opts.MaxStorageSizeMB,
		CPUCount:             cpuCount,
		MemoryMb:             memoryMB,
		BootTimeoutMs:        bootTimeoutMs,
		NetworkingMode:       networkingModeNAT,
	}

	var compatPtr unsafe.Pointer
	hr := vtblCall(m.ptr, slotCompatManagerCreateSession,
		uintptr(unsafe.Pointer(&settings)),
		uintptr(sessionFlagNone),
		0, // warning callback
		uintptr(unsafe.Pointer(&compatPtr)))
	runtime.KeepAlive(dnPtr)
	runtime.KeepAlive(spPtr)
	runtime.KeepAlive(settings)
	if err := hrErr(hr); err != nil {
		return nil, fmt.Errorf("wslcsession: CreateSession: %w", abiError(err, m.version))
	}

	// The session object implements the private interface too, and the two
	// methods this library cannot reach any other way are on it. Both
	// pointers are references to that one object, so both are released
	// together.
	privatePtr, err := queryInterface(compatPtr, &iidIWSLCSession)
	if err != nil {
		comRelease(compatPtr)
		return nil, fmt.Errorf("wslcsession: QueryInterface(IWSLCSession): %w", err)
	}

	return &comSession{
		compat:  compatPtr,
		private: privatePtr,
		version: m.version,
		slots:   slotsForVersion(m.version),
	}, nil
}

type comSession struct {
	compat  unsafe.Pointer
	private unsafe.Pointer
	version wslcVersion
	slots   sessionSlots
}

func (s *comSession) Release() {
	comRelease(s.private)
	comRelease(s.compat)
}

func (s *comSession) Terminate() error {
	return hrErr(vtblCall(s.compat, slotCompatSessionTerminate))
}

// MountWindowsFolder is one of the two private calls. The SDK-facing
// interface mounts a Windows directory only into a container
// (WSLCCompatVolume), never into the VM's own namespace, which is where the
// relay and the bridge have to find it.
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

	hr := vtblCall(s.private, s.slots.mountWindowsFolder,
		uintptr(unsafe.Pointer(wPtr)), uintptr(unsafe.Pointer(lPtr)), ro, acquireVmLease)
	runtime.KeepAlive(wPtr)
	runtime.KeepAlive(lPtr)
	return abiError(hrErr(hr), s.version)
}

// CreateRootNamespaceProcess is the other private call: the SDK-facing
// interface can start a process only inside a container (Exec, or a
// container's init process), and everything this library runs in the guest
// runs in the VM's root namespace instead.
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
	hr := vtblCall(s.private, s.slots.createRootNamespaceProcess,
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

	// Only the private interface can start this process, but nothing private
	// needs to be called on it afterwards, so the reference kept is the
	// SDK-facing one. Both refer to the same object: its keep-alive token
	// (and with it the VM) lives as long as either reference does, so
	// dropping the private one here changes nothing but which vtable
	// GetStdHandle is read from.
	compatProcess, qiErr := queryInterface(processPtr, &iidIWSLCCompatProcess)
	comRelease(processPtr)
	if qiErr != nil {
		return nil, &GuestExecError{
			Err:   fmt.Errorf("wslcsession: QueryInterface(IWSLCCompatProcess): %w", qiErr),
			Errno: errNo,
		}
	}
	return &comProcess{ptr: compatProcess}, nil
}

// CreateVolume calls IWSLCCompatSession::CreateVolume with the "vhd" driver
// (see WSLCVhdVolume.cpp / WSLCVolumeMetadata.h) to create a named docker
// volume backed by its own .vhdx, independent of the session's main storage.
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

	driverOpts := []compatKeyValuePair{
		{Key: sizeBytesKeyPtr, Value: sizeBytesValPtr},
		{Key: fixedKeyPtr, Value: fixedValPtr},
	}

	options := compatVolumeOptions{
		Name:            namePtr,
		Driver:          driverPtr,
		DriverOpts:      &driverOpts[0],
		DriverOptsCount: uint32(len(driverOpts)),
	}

	var info compatVolumeInformation
	hr := vtblCall(s.compat, slotCompatSessionCreateVolume,
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
	var h compatHandle
	hr := vtblCall(p.ptr, slotCompatProcessGetStdHandle, uintptr(fd), uintptr(unsafe.Pointer(&h)))
	if err := hrErr(hr); err != nil {
		return 0, err
	}
	// Observed to always be Socket in practice, but WSLCCompatHandle is a
	// genuine discriminated union - fail clearly here rather than silently
	// handing a File or Pipe handle to raw Winsock recv()/send() later,
	// which would misbehave instead of erroring.
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
