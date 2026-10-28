//go:build !linux

package rawsecrets

import "errors"

// onMemory refuses every directory: only a Linux sandbox can say its /run is
// a tmpfs, and a value is never written where that is not known.
func onMemory(string) error {
	return errors.New("only a Linux sandbox keeps raw secrets")
}
