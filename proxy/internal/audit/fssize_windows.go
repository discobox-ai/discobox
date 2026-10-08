package audit

import "golang.org/x/sys/windows"

// filesystemSize is the total size of the volume holding path.
func filesystemSize(path string) (int64, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(name, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return int64(total), nil
}
