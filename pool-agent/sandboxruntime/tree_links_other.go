//go:build !unix

package sandboxruntime

import "os"

// linkIndex is the no-op form for platforms with no inode to key on. Hard links
// are stored as separate files, which is correct but larger; a pool host is
// always Linux, so this exists to keep the package building rather than to be
// used.
type linkIndex struct{}

func newLinkIndex() *linkIndex { return &linkIndex{} }

func (l *linkIndex) seen(os.FileInfo, string) (string, bool) { return "", false }
