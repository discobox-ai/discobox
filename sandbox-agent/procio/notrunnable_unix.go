//go:build !windows

package procio

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// notRunnable is err, from starting opts, as a NotRunnableError when it means
// the program itself could not be run, and err unchanged otherwise.
//
// The child enters opts.Dir before it execs the program, and both report
// through the same "fork/exec" error: os/exec names a failed "chdir" only for
// a process with no SysProcAttr, and the caller may give one. So the errno
// alone cannot say which failed, and what is on disk decides: a working
// directory that is there, with a program that is not, is the program. The
// caller may start the program as another user than this process, so a
// permission error is the program's only when the program could not be
// executed by anyone; one that user alone is refused, in the program's
// directories or the working directory, stays the error it is.
//
// A file the kernel will not execute (ENOEXEC) is reported as one: a shell
// would run a file with no "#!" line as a script of its own, and this runs
// programs, not scripts.
func notRunnable(opts Options, err error) error {
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "fork/exec" {
		return err
	}
	if errors.Is(err, syscall.ENOEXEC) {
		return &NotRunnableError{Code: NotExecutableExit, Reason: "cannot execute: exec format error", Err: err}
	}
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return err
	}
	program := opts.Command[0]
	if !filepath.IsAbs(program) {
		program = filepath.Join(dir, program)
	}
	info, statErr := os.Stat(program)
	switch {
	case errors.Is(err, fs.ErrNotExist) && errors.Is(statErr, fs.ErrNotExist):
		return &NotRunnableError{Code: NotFoundExit, Reason: "no such file or directory", Err: err}
	case errors.Is(err, fs.ErrNotExist) && statErr == nil:
		// The program is there, so what was not found is what it names to
		// run it: a script's interpreter, a binary's loader.
		return &NotRunnableError{Code: NotExecutableExit, Reason: "cannot execute: required file not found", Err: err}
	case errors.Is(err, fs.ErrPermission) && statErr == nil && (info.IsDir() || info.Mode()&0o111 == 0):
		return &NotRunnableError{Code: NotExecutableExit, Reason: "permission denied", Err: err}
	}
	return err
}
