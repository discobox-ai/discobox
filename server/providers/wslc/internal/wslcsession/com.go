//go:build windows

package wslcsession

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"
)

// GUID mirrors the Windows GUID/CLSID/IID binary layout.
type GUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

func mustGUID(s string) GUID {
	g, err := parseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

func parseGUID(s string) (GUID, error) {
	var g GUID
	s = strings.Trim(s, "{}")
	parts := strings.Split(s, "-")
	if len(parts) != 5 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}

	decode := func(h string) ([]byte, error) { return hex.DecodeString(h) }

	d1, err := decode(parts[0])
	if err != nil || len(d1) != 4 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}
	d2, err := decode(parts[1])
	if err != nil || len(d2) != 2 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}
	d3, err := decode(parts[2])
	if err != nil || len(d3) != 2 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}
	d4a, err := decode(parts[3])
	if err != nil || len(d4a) != 2 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}
	d4b, err := decode(parts[4])
	if err != nil || len(d4b) != 6 {
		return g, fmt.Errorf("wslcsession: invalid GUID %q", s)
	}

	g.Data1 = binary.BigEndian.Uint32(d1)
	g.Data2 = binary.BigEndian.Uint16(d2)
	g.Data3 = binary.BigEndian.Uint16(d3)
	copy(g.Data4[0:2], d4a)
	copy(g.Data4[2:8], d4b)
	return g, nil
}

// hresultError wraps a raw HRESULT so callers can format/compare it.
type hresultError int32

func (h hresultError) Error() string {
	return fmt.Sprintf("HRESULT 0x%08X", uint32(h))
}

func (h hresultError) code() uint32 { return uint32(h) }

// Well-known HRESULTs, defined as their natural uint32 form (as written in
// Win32 headers) and converted via bit-reinterpretation rather than manual
// two's-complement arithmetic, which is easy to get subtly wrong by hand.
const (
	hrErrorAlreadyExists uint32 = 0x800700B7 // HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS); MountWindowsFolder returns this if already mounted.
	hrRPCETooLate        uint32 = 0x80010119 // RPC_E_TOO_LATE; CoInitializeSecurity was already called by something else in this process.
	hrRPCXBadStubData    uint32 = 0x800706F7 // HRESULT_FROM_WIN32(RPC_X_BAD_STUB_DATA); the service unmarshalled this call as a layout it did not arrive in.
)

func isHRESULT(err error, code uint32) bool {
	var he hresultError
	return errors.As(err, &he) && he.code() == code
}

func hrErr(hr uintptr) error {
	v := int32(hr)
	if v >= 0 {
		return nil
	}
	return hresultError(v)
}

const (
	clsctxLocalServer = 0x4

	rpcCAuthnLevelDefault   = 0
	rpcCImpLevelImpersonate = 3
	eoacStaticCloaking      = 0x20
)

var (
	ole32  = syscall.NewLazyDLL("ole32.dll")
	ws2_32 = syscall.NewLazyDLL("ws2_32.dll")

	procCoInitializeEx       = ole32.NewProc("CoInitializeEx")
	procCoInitializeSecurity = ole32.NewProc("CoInitializeSecurity")
	procCoCreateInstance     = ole32.NewProc("CoCreateInstance")

	procWSAStartup  = ws2_32.NewProc("WSAStartup")
	procRecv        = ws2_32.NewProc("recv")
	procSend        = ws2_32.NewProc("send")
	procShutdown    = ws2_32.NewProc("shutdown")
	procClosesocket = ws2_32.NewProc("closesocket")
)

// initCOM must run once, before any COM activation. Order matters:
// CoInitializeEx first, then CoInitializeSecurity with
// RPC_C_IMP_LEVEL_IMPERSONATE - the WSLService impersonates the caller to
// read its token/SID (see WSLCSessionManager.cpp's CreateSession), and
// without explicit impersonate-level security the default level Go/COM
// picks is too low, causing CreateSession to fail deep inside with an
// unrelated-looking 0x80070542 (ERROR_UNKNOWN_REVISION) from a garbled SID
// read. WSAStartup is needed separately because process stdio handles come
// back from the service as raw Winsock SOCKET handles (system_handle(sh_socket)
// in the IDL), and unmarshaling those throws WSAENOTINITIALISED if Winsock
// was never initialized in this process.
func initCOM() error {
	const coinitApartmentThreaded = 0x2
	r, _, _ := procCoInitializeEx.Call(0, coinitApartmentThreaded)
	// S_OK (0) or S_FALSE (1, already initialized) are both fine.
	if int32(r) < 0 {
		return hrErr(r)
	}

	r, _, _ = procCoInitializeSecurity.Call(
		0, ^uintptr(0), 0, 0,
		rpcCAuthnLevelDefault, rpcCImpLevelImpersonate, 0, eoacStaticCloaking, 0)
	// RPC_E_TOO_LATE means some other code in this process already called
	// CoInitializeSecurity - harmless as long as it set a sufficient level.
	if err := hrErr(r); err != nil && !isHRESULT(err, hrRPCETooLate) {
		return err
	}

	var wsaData [512]byte
	if ret, _, _ := procWSAStartup.Call(0x0202, uintptr(unsafe.Pointer(&wsaData[0]))); ret != 0 {
		return fmt.Errorf("wslcsession: WSAStartup failed: %d", ret)
	}

	return nil
}

func coCreateInstance(clsid, iid *GUID, clsctx uint32) (unsafe.Pointer, error) {
	var ppv unsafe.Pointer
	r, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(clsid)),
		0,
		uintptr(clsctx),
		uintptr(unsafe.Pointer(iid)),
		uintptr(unsafe.Pointer(&ppv)))
	if err := hrErr(r); err != nil {
		return nil, err
	}
	return ppv, nil
}

// vtblCall invokes the method at the given absolute vtable slot (0-based,
// counting IUnknown's QueryInterface=0/AddRef=1/Release=2), passing obj as
// the implicit `this` first argument. COM's x64 calling convention is a
// plain flat set of register/stack args, identical to what SyscallN expects,
// so no per-method trampoline is needed - just the right slot index and
// argument list.
//
// obj remains an unsafe.Pointer for its whole lifetime so vtable addressing
// never converts an integer back into a pointer. COM owns both obj and its
// vtable; unsafe.Add expresses the slot offset without hiding that ownership
// from Go's pointer analysis.
func vtblCall(obj unsafe.Pointer, slot int, args ...uintptr) uintptr {
	vtbl := *(*unsafe.Pointer)(obj)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(slot)*unsafe.Sizeof(uintptr(0))))
	full := make([]uintptr, 0, len(args)+1)
	full = append(full, uintptr(obj))
	full = append(full, args...)
	r, _, _ := syscall.SyscallN(fn, full...)
	return r
}

// queryInterface calls IUnknown::QueryInterface (vtable slot 0) and returns
// another reference to the same object, addressed through the requested
// interface. The WSL service's session and process objects each implement
// both the SDK-facing and the private interface, so this is what lets a call
// be made on whichever of the two is the safer place for it (see types.go).
//
// The returned pointer is a reference in its own right: QueryInterface
// AddRefs, so it has to be released separately from the one it was derived
// from.
func queryInterface(obj unsafe.Pointer, iid *GUID) (unsafe.Pointer, error) {
	var out unsafe.Pointer
	hr := vtblCall(obj, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if err := hrErr(hr); err != nil {
		return nil, err
	}
	return out, nil
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		vtblCall(obj, 2) // IUnknown::Release
	}
}

func recv(handle socketHandle, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	r, _, err := procRecv.Call(uintptr(handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	n := int(int32(r))
	if n < 0 {
		return 0, fmt.Errorf("wslcsession: recv failed: %w", err)
	}
	return n, nil
}

func send(handle socketHandle, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	r, _, err := procSend.Call(uintptr(handle), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
	n := int(int32(r))
	if n < 0 {
		return 0, fmt.Errorf("wslcsession: send failed: %w", err)
	}
	return n, nil
}

// shutdownSocket and closesocket are best-effort: called from Close()/
// CloseWrite() paths where the socket may already be half-torn-down, and
// there's nothing meaningful to do with a failure at that point.
func shutdownSocket(handle socketHandle, how int) {
	_, _, _ = procShutdown.Call(uintptr(handle), uintptr(how))
}

func closesocket(handle socketHandle) {
	_, _, _ = procClosesocket.Call(uintptr(handle))
}
