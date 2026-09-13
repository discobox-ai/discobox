package wslcsession

import "fmt"

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
