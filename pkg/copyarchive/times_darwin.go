package copyarchive

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

type darwinTimeAPI struct {
	set   func(int, *unix.Attrlist, unsafe.Pointer, uintptr, uint32) int32
	errno func() *int32
}

// Use libSystem rather than a deprecated direct syscall, including in the
// CGO-disabled CLI. The library remains loaded for the process lifetime.
var loadDarwinTimeAPI = sync.OnceValues(func() (*darwinTimeAPI, error) {
	handle, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, err
	}
	setter, err := purego.Dlsym(handle, "fsetattrlist")
	if err != nil {
		return nil, err
	}
	errno, err := purego.Dlsym(handle, "__error")
	if err != nil {
		return nil, err
	}
	api := &darwinTimeAPI{}
	purego.RegisterFunc(&api.set, setter)
	purego.RegisterFunc(&api.errno, errno)
	return api, nil
})

// fsetattrlist applies nanosecond timestamps to the pinned object. futimes
// carries only microseconds; applying times by path would reintroduce a race.
func setTimes(f *os.File, a, m time.Time) error {
	api, err := loadDarwinTimeAPI()
	if err != nil {
		return fmt.Errorf("load file timestamp API: %w", err)
	}
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_MODTIME | unix.ATTR_CMN_ACCTIME}
	// Common attributes are packed in bitmap order: modification, then access.
	times := [2]unix.Timespec{{Sec: m.Unix(), Nsec: int64(m.Nanosecond())}, {Sec: a.Unix(), Nsec: int64(a.Nanosecond())}}
	// errno is thread-local, so the call and its error read stay together.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	result := api.set(int(f.Fd()), &attrs, unsafe.Pointer(&times), unsafe.Sizeof(times), 0)
	var callErr error
	if result != 0 {
		callErr = unix.Errno(*api.errno())
	}
	runtime.KeepAlive(f)
	runtime.KeepAlive(attrs)
	runtime.KeepAlive(times)
	return callErr
}
