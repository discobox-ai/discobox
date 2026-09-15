//go:build !windows

package wslc

// growStorageDisk has nothing to grow off Windows, where no wslc VM can run.
func growStorageDisk(string, uint64) (uint64, bool, error) {
	return 0, false, nil
}
