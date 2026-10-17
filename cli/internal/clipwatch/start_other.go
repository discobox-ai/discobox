//go:build !windows && !linux

package clipwatch

import "context"

// Start returns a Watcher that never goes live: there is no Windows clipboard
// here, and reading this platform's own when the key arrives is fast enough.
func Start(context.Context) *Watcher {
	w := NewWatcher()
	w.fail(ErrUnsupported)
	return w
}
