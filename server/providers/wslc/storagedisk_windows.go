//go:build windows

package wslc

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	virtdisk                      = windows.NewLazySystemDLL("virtdisk.dll")
	procOpenVirtualDisk           = virtdisk.NewProc("OpenVirtualDisk")
	procGetVirtualDiskInformation = virtdisk.NewProc("GetVirtualDiskInformation")
	procResizeVirtualDisk         = virtdisk.NewProc("ResizeVirtualDisk")
)

// virtualStorageType mirrors VIRTUAL_STORAGE_TYPE. The zero value is
// VIRTUAL_STORAGE_TYPE_DEVICE_UNKNOWN from an unknown vendor, which has the
// API pick the format from the file itself.
type virtualStorageType struct {
	DeviceID uint32
	VendorID windows.GUID
}

// openVirtualDiskParametersV2 mirrors OPEN_VIRTUAL_DISK_PARAMETERS at
// OPEN_VIRTUAL_DISK_VERSION_2, the version ResizeVirtualDisk requires.
type openVirtualDiskParametersV2 struct {
	Version        uint32
	GetInfoOnly    int32
	ReadOnly       int32
	ResiliencyGUID windows.GUID
}

// getVirtualDiskInfoSize mirrors GET_VIRTUAL_DISK_INFO for
// GET_VIRTUAL_DISK_INFO_SIZE, the only member read here.
type getVirtualDiskInfoSize struct {
	Version      uint32
	_            uint32
	VirtualSize  uint64
	PhysicalSize uint64
	BlockSize    uint32
	SectorSize   uint32
}

// resizeVirtualDiskParameters mirrors RESIZE_VIRTUAL_DISK_PARAMETERS at
// RESIZE_VIRTUAL_DISK_VERSION_1.
type resizeVirtualDiskParameters struct {
	Version uint32
	_       uint32
	NewSize uint64
}

const (
	openVirtualDiskVersion2   = 2
	getVirtualDiskInfoSizeVer = 1
	resizeVirtualDiskVersion1 = 1
)

// growStorageDisk raises the virtual size of an existing VHDX to size bytes
// and reports the size it had. wslc reads a pool's maximum storage size only
// when it creates the disk, so without this a raised limit would never reach a
// pool that already has one.
//
// It never shrinks, and a disk that does not exist yet is left for wslc to
// create at the configured size.
func growStorageDisk(path string, size uint64) (previous uint64, grown bool, err error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, false, err
	}
	params := openVirtualDiskParametersV2{Version: openVirtualDiskVersion2}
	var handle windows.Handle
	if rc, _, _ := procOpenVirtualDisk.Call(
		uintptr(unsafe.Pointer(&virtualStorageType{})),
		uintptr(unsafe.Pointer(pathPtr)),
		0, // VIRTUAL_DISK_ACCESS_NONE, required by version 2
		0, // OPEN_VIRTUAL_DISK_FLAG_NONE
		uintptr(unsafe.Pointer(&params)),
		uintptr(unsafe.Pointer(&handle)),
	); rc != 0 {
		return 0, false, fmt.Errorf("open %s: %w", path, windows.Errno(rc))
	}
	defer func() { _ = windows.CloseHandle(handle) }()

	info := getVirtualDiskInfoSize{Version: getVirtualDiskInfoSizeVer}
	infoSize := uint32(unsafe.Sizeof(info))
	if rc, _, _ := procGetVirtualDiskInformation.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(&infoSize)),
		uintptr(unsafe.Pointer(&info)),
		0,
	); rc != 0 {
		return 0, false, fmt.Errorf("read size of %s: %w", path, windows.Errno(rc))
	}
	if info.VirtualSize >= size {
		return info.VirtualSize, false, nil
	}

	resize := resizeVirtualDiskParameters{Version: resizeVirtualDiskVersion1, NewSize: size}
	if rc, _, _ := procResizeVirtualDisk.Call(
		uintptr(handle),
		0, // RESIZE_VIRTUAL_DISK_FLAG_NONE
		uintptr(unsafe.Pointer(&resize)),
		0, // synchronous
	); rc != 0 {
		return info.VirtualSize, false, fmt.Errorf("grow %s from %d to %d bytes: %w", path, info.VirtualSize, size, windows.Errno(rc))
	}
	return info.VirtualSize, true, nil
}
