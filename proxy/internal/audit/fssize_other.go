//go:build !unix && !windows

package audit

import "errors"

// filesystemSize cannot size a filesystem on this platform, so only an
// absolute spool budget applies here.
func filesystemSize(string) (int64, error) {
	return 0, errors.New("filesystem size is not available on this platform")
}
