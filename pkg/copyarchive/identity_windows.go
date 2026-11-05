package copyarchive

import (
	"fmt"
	"io/fs"

	"golang.org/x/sys/windows"
)

// FileIdentity identifies the volume and file index without opening content.
func FileIdentity(name string, _ fs.FileInfo) (string, error) {
	path, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	handle, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x:%x:%x", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow), nil
}
