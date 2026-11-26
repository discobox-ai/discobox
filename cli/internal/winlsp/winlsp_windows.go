//go:build windows

package winlsp

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ws2_32                        = windows.NewLazySystemDLL("ws2_32.dll")
	procWSCGetApplicationCategory = ws2_32.NewProc("WSCGetApplicationCategory")
	procWSCSetApplicationCategory = ws2_32.NewProc("WSCSetApplicationCategory")

	shell32             = windows.NewLazySystemDLL("shell32.dll")
	procShellExecuteExW = shell32.NewProc("ShellExecuteExW")
)

// wsaServiceNotFound is WSASERVICE_NOT_FOUND: no category is recorded for the
// path, which is the same as an empty one.
const wsaServiceNotFound = 10108

// Category is the LSP category Windows records for the executable at path,
// matched with no command-line arguments, which is how the CLI starts the
// server. Reading it needs no elevation.
func Category(path string) (uint32, error) {
	p, n, err := utf16Path(path)
	if err != nil {
		return 0, err
	}
	var category uint32
	var errno int32
	r, _, _ := procWSCGetApplicationCategory.Call(
		uintptr(unsafe.Pointer(p)), uintptr(n),
		0, 0,
		uintptr(unsafe.Pointer(&category)),
		uintptr(unsafe.Pointer(&errno)))
	if int32(r) != 0 {
		if errno == wsaServiceNotFound {
			return 0, nil
		}
		return 0, fmt.Errorf("WSCGetApplicationCategory: %w", windows.Errno(errno))
	}
	return category, nil
}

// SetCategory records category for the executable at path. It writes HKLM,
// so it needs an elevated token.
func SetCategory(path string, category uint32) error {
	p, n, err := utf16Path(path)
	if err != nil {
		return err
	}
	var previous uint32
	var errno int32
	r, _, _ := procWSCSetApplicationCategory.Call(
		uintptr(unsafe.Pointer(p)), uintptr(n),
		0, 0,
		uintptr(category),
		uintptr(unsafe.Pointer(&previous)),
		uintptr(unsafe.Pointer(&errno)))
	if int32(r) != 0 {
		return fmt.Errorf("WSCSetApplicationCategory: %w", windows.Errno(errno))
	}
	return nil
}

// utf16Path is path as the WSC calls take it: a NUL-terminated UTF-16 string
// and its length in characters, without the NUL.
func utf16Path(path string) (*uint16, uint32, error) {
	encoded, err := windows.UTF16FromString(path)
	if err != nil {
		return nil, 0, err
	}
	return &encoded[0], uint32(len(encoded) - 1), nil
}

// Elevated reports whether this process holds an elevated token.
func Elevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// shellExecuteInfo is SHELLEXECUTEINFOW. Go lays it out as C does on every
// Windows architecture Go builds for.
type shellExecuteInfo struct {
	size          uint32
	mask          uint32
	hwnd          windows.Handle
	verb          *uint16
	file          *uint16
	parameters    *uint16
	directory     *uint16
	show          int32
	instApp       windows.Handle
	idList        uintptr
	class         *uint16
	keyClass      windows.Handle
	hotKey        uint32
	iconOrMonitor windows.Handle
	process       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	seeMaskFlagNoUI       = 0x00000400
	swHide                = 0
)

// RunElevated starts exe with args through the "runas" verb, which shows the
// UAC prompt, waits for it to exit, and returns its exit code. The window is
// hidden: the caller reports what happened, by reading back what the
// elevated process was to change.
func RunElevated(exe string, args []string) (uint32, error) {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = windows.EscapeArg(arg)
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return 0, err
	}
	parameters, err := windows.UTF16PtrFromString(strings.Join(quoted, " "))
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{
		mask:       seeMaskNoCloseProcess | seeMaskNoAsync | seeMaskFlagNoUI,
		verb:       verb,
		file:       file,
		parameters: parameters,
		show:       swHide,
	}
	info.size = uint32(unsafe.Sizeof(info))
	if r, _, err := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errors.Is(err, windows.ERROR_CANCELLED) { //nolint:misspell // Windows's own name for it.
			return 0, ErrDeclined
		}
		return 0, fmt.Errorf("start %s elevated: %w", exe, err)
	}
	if info.process == 0 {
		return 0, fmt.Errorf("start %s elevated: no process was started", exe)
	}
	defer func() { _ = windows.CloseHandle(info.process) }()
	if _, err := windows.WaitForSingleObject(info.process, windows.INFINITE); err != nil {
		return 0, fmt.Errorf("wait for the elevated %s: %w", exe, err)
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.process, &code); err != nil {
		return 0, fmt.Errorf("read the elevated %s's exit status: %w", exe, err)
	}
	return code, nil
}
