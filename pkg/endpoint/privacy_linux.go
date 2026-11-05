package endpoint

import (
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

// Linux folds effective POSIX ACL grants into the group mode mask.
func checkDirectoryACL(int) error { return nil }

// Keep the full ACL in the consent snapshot, including currently masked grants.
func repairDirectoryACL(fd int) (string, error) {
	size, err := unix.Fgetxattr(fd, "system.posix_acl_access", nil)
	if err == unix.ENODATA || err == unix.ENOTSUP {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("cannot inspect directory ACL: %w", err)
	}
	if size < 4 || size > 65536 {
		return "", fmt.Errorf("invalid directory ACL size")
	}
	data := make([]byte, size)
	n, err := unix.Fgetxattr(fd, "system.posix_acl_access", data)
	if err != nil {
		return "", fmt.Errorf("cannot read directory ACL: %w", err)
	}
	data = data[:n]
	if n < 4 || (n-4)%8 != 0 || binary.LittleEndian.Uint32(data) != 2 {
		return "", fmt.Errorf("invalid directory ACL")
	}
	mask := uint16(7)
	for pos := 4; pos < n; pos += 8 {
		if binary.LittleEndian.Uint16(data[pos:]) == 16 {
			mask = binary.LittleEndian.Uint16(data[pos+2:])
		}
	}
	for pos := 4; pos < n; pos += 8 {
		tag := binary.LittleEndian.Uint16(data[pos:])
		perm := binary.LittleEndian.Uint16(data[pos+2:])
		if (tag == 2 || tag == 8) && perm&mask != 0 {
			return "", fmt.Errorf("directory ACL grants access to another user or group; inspect and repair it manually")
		}
	}
	return string(data), nil
}
