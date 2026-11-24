package procio

// notRunnable returns err unchanged. Telling a program that could not be run
// from any other failed start is read from POSIX errnos and file modes, which
// Windows does not report the same way, and a program a shim runs is in the
// Linux sandbox.
func notRunnable(_ Options, err error) error {
	return err
}
