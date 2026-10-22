package files

import (
	"io/fs"
	"strconv"
)

// version tags what a path holds now, by its modification time and size:
// Windows reports no inode through a FileInfo.
func version(info fs.FileInfo) string {
	return strconv.FormatInt(info.ModTime().UnixNano(), 36) + "-" + strconv.FormatInt(info.Size(), 36)
}
