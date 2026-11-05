package endpoint

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// Values and iteration semantics are Apple's sys/acl.h and acl_entry.c:
// https://github.com/apple-oss-distributions/Libc/blob/main/include/sys/acl.h
const (
	aclFirstEntry int32 = 0
	aclNextEntry  int32 = -1
	aclDeny       int32 = 2
	aclMaxEntries       = 128
)

// Darwin evaluates ACL grants before BSD mode bits. Even mode 0700 therefore
// needs an ACL check, on ancestors too: an ACL must not let another user replace
// a directory whose descendants we checked. Deny entries only restrict access;
// reject allow entries rather than trying to reconstruct group membership.
var directoryACL = sync.OnceValues(func() (func(int) (string, error), error) {
	library, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	var getFD func(int32) uintptr
	var getEntry func(uintptr, int32, *uintptr) int32
	var getTag func(uintptr, *int32) int32
	var free func(uintptr) int32
	var toText func(uintptr, *int64) *byte
	var freeText func(*byte) int32
	var errno func() *int32
	purego.RegisterLibFunc(&getFD, library, "acl_get_fd")
	purego.RegisterLibFunc(&getEntry, library, "acl_get_entry")
	purego.RegisterLibFunc(&getTag, library, "acl_get_tag_type")
	purego.RegisterLibFunc(&free, library, "acl_free")
	purego.RegisterLibFunc(&toText, library, "acl_to_text")
	purego.RegisterLibFunc(&freeText, library, "acl_free")
	purego.RegisterLibFunc(&errno, library, "__error")
	return func(fd int) (string, error) {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		acl := getFD(int32(fd))
		if acl == 0 {
			if *errno() == int32(unix.ENOENT) {
				return "", nil
			}
			return "", fmt.Errorf("cannot inspect directory ACL: %w", unix.Errno(*errno()))
		}
		defer free(acl)
		var length int64
		text := toText(acl, &length)
		if text == nil {
			return "", fmt.Errorf("cannot snapshot directory ACL")
		}
		defer freeText(text)
		if length < 0 || length > 1<<20 {
			return "", fmt.Errorf("directory ACL text exceeds limit")
		}
		snapshot := string(unsafe.Slice(text, int(length)))
		var entry uintptr
		// Apple's API returns 0 for an entry and -1 at the end (unlike Linux).
		which := aclFirstEntry
		for count := 0; ; count++ {
			if getEntry(acl, which, &entry) != 0 {
				if *errno() == int32(unix.EINVAL) {
					return snapshot, nil
				}
				return "", fmt.Errorf("cannot read directory ACL entry")
			}
			if count >= aclMaxEntries {
				return "", fmt.Errorf("directory ACL exceeds entry limit")
			}
			which = aclNextEntry
			var tag int32
			if getTag(entry, &tag) != 0 {
				return "", fmt.Errorf("cannot inspect directory ACL entry")
			}
			if tag != aclDeny {
				return "", fmt.Errorf("directory ACL grants access beyond its private mode permissions")
			}
		}
	}, nil
})

func checkDirectoryACL(fd int) error {
	_, err := repairDirectoryACL(fd)
	return err
}

func repairDirectoryACL(fd int) (string, error) {
	check, err := directoryACL()
	if err != nil {
		return "", fmt.Errorf("inspect directory ACL: %w", err)
	}
	return check(fd)
}
