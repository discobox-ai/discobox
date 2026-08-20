//go:build windows

package localpty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// minBuild is Windows 10 1809, the first build with a pseudo-console. Below it
// there is nothing to fall back to that would still be a terminal, so Start
// says so instead. See ADR 0065.
const minBuild = 17763

// waitForExit is how long Close waits for a terminated child before releasing
// the console anyway. Terminating is immediate; this is only a bound on a
// process that will not go, so the window is never held by one.
const waitForExit = 5000 // milliseconds

// start creates a pseudo-console and runs the command attached to it.
//
// os/exec cannot do this. A pseudo-console reaches a child as a thread
// attribute in a STARTUPINFOEX (PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE), and
// syscall.SysProcAttr has no attribute list to put it in — so the process is
// started here, command line, environment and all.
func start(ctx context.Context, cmd Command, cols, rows int) (PTY, error) {
	cols, rows = size(cols, rows)
	if build := windows.RtlGetVersion().BuildNumber; build < minBuild {
		return nil, fmt.Errorf("this Windows has no pseudo-console: build %d, and %d (Windows 10 1809) is the first that has one", build, minBuild)
	}

	// Two pipes. The console is handed the read end of one and the write end of
	// the other; what is kept here is the far end of each — the one this
	// process writes keys to, and the one it reads the terminal's output from.
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, fmt.Errorf("open the input pipe: %w", err)
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		closeHandles(inRead, inWrite)
		return nil, fmt.Errorf("open the output pipe: %w", err)
	}

	var console windows.Handle
	err := windows.CreatePseudoConsole(coord(cols, rows), inRead, outWrite, 0, &console)
	// The console holds its own duplicates of the two ends it was given, so
	// this process is done with them whether or not that succeeded — and
	// keeping the output pipe's write end open here would be keeping a reader
	// from ever seeing the command finish.
	closeHandles(inRead, outWrite)
	if err != nil {
		closeHandles(inWrite, outRead)
		return nil, fmt.Errorf("create the pseudo-console: %w", err)
	}

	process, err := startOnConsole(console, cmd)
	if err != nil {
		windows.ClosePseudoConsole(console)
		closeHandles(inWrite, outRead)
		return nil, err
	}

	p := &conPTY{
		console: console,
		process: process,
		in:      os.NewFile(uintptr(inWrite), "conpty-in"),
		out:     os.NewFile(uintptr(outRead), "conpty-out"),
		waited:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	go p.watchExit(process)
	// What exec.CommandContext would have done, by hand: os/exec is not
	// involved here, so nothing else is watching the context.
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()
	return p, nil
}

// startOnConsole starts the command with the pseudo-console attached, and
// returns the process handle.
func startOnConsole(console windows.Handle, cmd Command) (windows.Handle, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return 0, fmt.Errorf("allocate the thread attributes: %w", err)
	}
	defer attrs.Delete()
	// The handle is the attribute's value rather than a pointer to it — which
	// is what the size says, and why the conversion is spelled through a
	// pointer to the local instead of casting the handle itself.
	if err := attrs.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&console)),
		unsafe.Sizeof(console),
	); err != nil {
		return 0, fmt.Errorf("attach the pseudo-console: %w", err)
	}

	var si windows.StartupInfoEx
	si.ProcThreadAttributeList = attrs.List()
	si.Cb = uint32(unsafe.Sizeof(si))
	// STARTF_USESTDHANDLES with all three handles left null, which is the one
	// arrangement that works and reads like the one that should not.
	//
	// Without the flag CreateProcess copies this process's own standard handles
	// into the child, and those outrank the console it was just attached to: the
	// pseudo-console is created, the child joins it — the title even comes back
	// through the pipe — and everything the command prints goes to whatever this
	// window's stdout happened to be. With the flag and no handles the child
	// starts with none, and the console it is attached to supplies them.
	si.StartupInfo.Flags |= windows.STARTF_USESTDHANDLES

	argv, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{cmd.Path}, cmd.Args...)))
	if err != nil {
		return 0, fmt.Errorf("build the command line: %w", err)
	}
	env, err := envBlock(cmd.Env)
	if err != nil {
		return 0, err
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return 0, fmt.Errorf("build the working directory: %w", err)
		}
	}

	var info windows.ProcessInformation
	// Nothing is inherited: the console reaches the child through the attribute
	// list, and a child that inherited this window's handles would be holding
	// pipes it has no idea about.
	if err := windows.CreateProcess(
		nil, argv, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		env, dir, &si.StartupInfo, &info,
	); err != nil {
		return 0, fmt.Errorf("start %s: %w", cmd.Path, err)
	}
	closeHandles(info.Thread)
	return info.Process, nil
}

// conPTY is a pseudo-console with a command running on it.
type conPTY struct {
	in  *os.File // keys, written to the console
	out *os.File // the terminal's output, read from it

	// mu guards the two handles, which are released by whichever of the command
	// exiting and the pane closing happens first.
	mu      sync.Mutex
	console windows.Handle
	process windows.Handle

	// waited is closed once the command has exited and code is its exit status.
	waited chan struct{}
	code   int

	// done ends the context watcher when the pty is closed for any other
	// reason, so a long-lived context does not hold a goroutine per command.
	done      chan struct{}
	closeOnce sync.Once
}

// ExitStatus is the command's exit code, once there is one. The wait is the
// interface's, and is over before it starts here: the code is collected before
// the console closes, and the console closing is what ends the read that sends
// anyone looking for it.
func (p *conPTY) ExitStatus() (int, bool) {
	select {
	case <-p.waited:
		return p.code, true
	case <-time.After(exitGrace):
		return 0, false
	}
}

// watchExit releases the console when the command finishes.
//
// This is the difference that would otherwise be visible to everything above:
// a Unix pty reports end of file when its last slave descriptor closes, and a
// Windows pseudo-console does not. It holds the write end of the output pipe
// until it is closed, so a command that has exited leaves a reader waiting on a
// pipe nothing will ever write to again — and a pane that never learns its
// command finished.
//
// Closing the console here flushes what the command last printed and ends the
// read. The process handle is this goroutine's to release, so nothing else can
// close it while the wait is on it.
func (p *conPTY) watchExit(process windows.Handle) {
	_, _ = windows.WaitForSingleObject(process, windows.INFINITE)
	// Collected before the console goes, so that the end of file the pane is
	// about to read is never ahead of the answer to how the command ended.
	var code uint32
	if err := windows.GetExitCodeProcess(process, &code); err == nil {
		p.code = int(int32(code))
	}
	close(p.waited)

	p.closeConsole()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.process != 0 {
		closeHandles(p.process)
		p.process = 0
	}
}

// closeConsole releases the pseudo-console, once, from whichever of the command
// exiting and the pane closing gets there first.
func (p *conPTY) closeConsole() {
	p.mu.Lock()
	console := p.console
	p.console = 0
	p.mu.Unlock()
	if console != 0 {
		windows.ClosePseudoConsole(console)
	}
}

// Read returns io.EOF where the pipe reports a broken one.
//
// A closed console leaves the output pipe with no writer, which is what a
// finished command looks like from here. Go's own pipe handling usually reaches
// io.EOF first; this is the case where the error arrives raw, and reporting it
// as a failure would put "The pipe has been ended" on screen every time a
// command completed normally.
func (p *conPTY) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if err != nil && errors.Is(err, windows.ERROR_BROKEN_PIPE) {
		return n, io.EOF
	}
	return n, err
}

func (p *conPTY) Write(b []byte) (int, error) { return p.in.Write(b) }

// Resize sets the console's size, which is what tells the child its window
// changed. A command that has already finished is not an error to resize: the
// pane outlives it, and a window rearranged over a finished command has nothing
// to tell it.
func (p *conPTY) Resize(cols, rows int) error {
	if cols <= 0 || rows <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.console == 0 {
		return nil
	}
	return windows.ResizePseudoConsole(p.console, coord(cols, rows))
}

// Close ends the command and releases the console.
//
// The order is the part that matters. ClosePseudoConsole does not return until
// the console has finished flushing its output, so it goes last, after the read
// end of the output pipe is closed: a flush into a pipe nobody is reading fails
// at once, where a flush nobody has drained waits forever — and waiting here is
// the window hanging on the key that dismissed the pane.
func (p *conPTY) Close() error {
	p.closeOnce.Do(func() {
		close(p.done)
		p.terminate()
		_ = p.in.Close()
		_ = p.out.Close()
		p.closeConsole()
	})
	return nil
}

// terminate ends the command if it is still running. The pane is going, and a
// command waiting on a key does not outlive it.
//
// The lock is held across the wait because the handle is closed under it: a
// terminate that raced the command's own exit would otherwise be terminating
// whatever Windows had since given the number to.
func (p *conPTY) terminate() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.process == 0 {
		return
	}
	_ = windows.TerminateProcess(p.process, 1)
	_, _ = windows.WaitForSingleObject(p.process, waitForExit)
}

// envBlock is a Windows environment block: every KEY=VALUE in UTF-16, each
// terminated, the whole run terminated again. os/exec builds one of these
// itself; here there is no os/exec.
//
// Nothing is a nil block, which is how CreateProcess is told to hand the child
// this process's own environment — the same answer exec.Cmd's nil Env gives.
func envBlock(env []string) (*uint16, error) {
	if len(env) == 0 {
		return nil, nil
	}
	var block []uint16
	for _, entry := range env {
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			// The only way this fails is an embedded NUL, which would end the
			// block early and silently drop everything after it.
			return nil, fmt.Errorf("environment entry is not usable: %w", err)
		}
		block = append(block, encoded...)
	}
	block = append(block, 0)
	return &block[0], nil
}

// coord is a terminal size as the console API takes it. The dimensions are
// screen cells, so they fit; a caller that asked for more than a terminal can
// be is clamped rather than wrapped around into a negative one.
func coord(cols, rows int) windows.Coord {
	const maxCell = 0x7fff
	//nolint:gosec // clamped above, and a cell count is not a signed overflow
	return windows.Coord{X: int16(min(cols, maxCell)), Y: int16(min(rows, maxCell))}
}

func closeHandles(handles ...windows.Handle) {
	for _, handle := range handles {
		if handle != 0 && handle != windows.InvalidHandle {
			_ = windows.CloseHandle(handle)
		}
	}
}

var (
	_ PTY          = (*conPTY)(nil)
	_ ExitReporter = (*conPTY)(nil)
)
