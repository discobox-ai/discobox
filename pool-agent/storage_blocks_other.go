//go:build !unix

package poolagent

import "os"

// statBlocks has no answer off unix, so allocated size falls back to apparent
// size. Windows hosts run their pools in a VM, so this affects no real pool.
func statBlocks(os.FileInfo) (int64, bool) {
	return 0, false
}

// statDevice has no answer off unix: every tree is taken to be on the
// filesystem statfs measured.
func statDevice(string) (uint64, bool) {
	return 0, false
}
