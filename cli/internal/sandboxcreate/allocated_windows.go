package sandboxcreate

import (
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

var compressedFileSize = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetCompressedFileSizeW")

func allocatedBytes(path string, info fs.FileInfo) int64 {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return info.Size()
	}
	var high uint32
	low, _, callErr := compressedFileSize.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&high)))
	if uint32(low) == 0xffffffff && callErr != windows.ERROR_SUCCESS {
		return info.Size()
	}
	return int64(uint64(high)<<32 | uint64(uint32(low)))
}
