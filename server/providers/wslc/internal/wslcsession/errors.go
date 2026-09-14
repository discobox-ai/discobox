package wslcsession

import (
	"errors"
	"fmt"
)

// ErrSessionExists reports that a VM of the requested name is already running.
//
// wslc keys sessions by display name and refuses a duplicate. A session belongs
// to the process that created it and dies with it, but not at once when that
// process is killed rather than closing it: the service ends a dead process's
// session only once it has noticed the process is gone, which takes seconds and
// can take minutes. So this usually means an earlier process has not been
// cleaned up after yet - not that anything is misconfigured - and a caller can
// match it to wait rather than treat the pool as failed.
var ErrSessionExists = errors.New("wslcsession: session already exists")

// GuestExecError wraps a CreateRootNamespaceProcess failure together with
// the guest-side errno reported alongside it. WSLCSession.cpp initializes
// the errno to -1 before attempting the exec, specifically "to make sure
// not to return 0 if something fails" - so Errno == -1 means "no specific
// errno available", not "success".
//
// It is defined on every platform, unlike the session that returns it,
// because the driver decides what a failed dial means without a build tag.
type GuestExecError struct {
	Err   error
	Errno int32
}

func (e *GuestExecError) Error() string { return fmt.Sprintf("%v (guest errno=%d)", e.Err, e.Errno) }
func (e *GuestExecError) Unwrap() error { return e.Err }
