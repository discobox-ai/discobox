//go:build !windows

package procio

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// A start that fails because of the program is a NotRunnableError carrying the
// shell's status for it; one that fails because of the working directory is
// not, even though the kernel reports both through the same exec error.
func TestStartTellsAProgramThatCannotRunFromAFailedStart(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	notExecutable := write("not-executable", "#!/bin/sh\n", 0o644)
	badFormat := write("bad-format", "\x00\x01not a program", 0o755)
	badInterpreter := write("bad-interpreter", "#!/nonexistent/sh\n", 0o755)
	cases := []struct {
		name    string
		command string
		dir     string
		code    int64
		reason  string
	}{
		{"not there", filepath.Join(dir, "missing"), dir, NotFoundExit, "no such file or directory"},
		{"not there, relative to the working directory", "./missing", dir, NotFoundExit, "no such file or directory"},
		{"not executable", notExecutable, dir, NotExecutableExit, "permission denied"},
		{"a directory", dir, dir, NotExecutableExit, "permission denied"},
		{"not a program", badFormat, dir, NotExecutableExit, "cannot execute: exec format error"},
		{"its interpreter not there", badInterpreter, dir, NotExecutableExit, "cannot execute: required file not found"},
		{"in a working directory that is not there", "/bin/true", filepath.Join(dir, "missing-dir"), 0, ""},
		{"not there, in a working directory that is not there", filepath.Join(dir, "missing"), filepath.Join(dir, "missing-dir"), 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proc, err := Start(Options{Command: []string{tc.command}, Dir: tc.dir, Env: os.Environ(), SysProcAttr: newSessionAttr()})
			if err == nil {
				proc.Close()
				t.Fatal("start succeeded")
			}
			notRunnable, ok := errors.AsType[*NotRunnableError](err)
			if tc.code == 0 {
				if ok {
					t.Fatalf("start error = %v, want a failed start, not the program's", err)
				}
				return
			}
			if !ok || notRunnable.Code != tc.code || notRunnable.Reason != tc.reason {
				t.Fatalf("start error = %#v, want %d %q", err, tc.code, tc.reason)
			}
		})
	}
}
