//go:build windows

package terminal

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// linkSkillDirectory makes link a directory junction to the skill directory
// target. A symbolic link needs a privilege the run user may not hold; a
// junction does not, and a directory is all a skill is (ADR 26-10-14-294 §3).
// A sandbox on Windows has no POSIX ids to give it.
//
// The junction is written as a reparse point, not by `mklink /J`: the skill's
// name comes from a repository, and cmd would read `&`, `%` or `^` in it as
// its own syntax.
func linkSkillDirectory(target, link string, _, _ *int64) error {
	target, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	if err := os.Mkdir(link, 0o755); err != nil {
		return err
	}
	if err := setJunction(link, target); err != nil {
		_ = os.Remove(link)
		return fmt.Errorf("make %s a junction to %s: %w", link, target, err)
	}
	return nil
}

// setJunction turns the empty directory link into a mount point reparse
// point naming target, which is what a junction is.
func setJunction(link, target string) error {
	path, err := windows.UTF16PtrFromString(link)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(path, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	buffer := mountPointReparseBuffer(`\??\`+target, target)
	var returned uint32
	return windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil)
}

// mountPointReparseBuffer is REPARSE_DATA_BUFFER for IO_REPARSE_TAG_MOUNT_POINT:
// the substitute name the file system follows and the print name it shows,
// each NUL-terminated, their lengths in bytes without the NUL.
func mountPointReparseBuffer(substitute, print string) []byte {
	sub := utf16.Encode([]rune(substitute))
	prn := utf16.Encode([]rune(print))
	names := make([]uint16, 0, len(sub)+len(prn)+2)
	names = append(append(append(append(names, sub...), 0), prn...), 0)
	const header = 8 // the four offsets and lengths after ReparseDataLength
	dataLength := header + 2*len(names)
	buffer := make([]byte, 8+dataLength)
	binary.LittleEndian.PutUint32(buffer[0:], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:], uint16(dataLength))
	binary.LittleEndian.PutUint16(buffer[8:], 0)
	binary.LittleEndian.PutUint16(buffer[10:], uint16(2*len(sub)))
	binary.LittleEndian.PutUint16(buffer[12:], uint16(2*(len(sub)+1)))
	binary.LittleEndian.PutUint16(buffer[14:], uint16(2*len(prn)))
	for i, unit := range names {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], unit)
	}
	return buffer
}
