//go:build !linux

package libkrun

// allocatedBlocks has no portable answer, so off Linux the sparseness of a disk
// image is simply not asserted. The provider does not run there.
func allocatedBlocks(string) (int64, bool) { return 0, false }
