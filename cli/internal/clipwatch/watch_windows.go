package clipwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")
	kernel32                       = windows.NewLazySystemDLL("kernel32.dll")
	procAddClipboardFormatListener = user32.NewProc("AddClipboardFormatListener")
	procCloseClipboard             = user32.NewProc("CloseClipboard")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procDestroyWindow              = user32.NewProc("DestroyWindow")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procGetClipboardData           = user32.NewProc("GetClipboardData")
	procGetClipboardSequenceNumber = user32.NewProc("GetClipboardSequenceNumber")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procIsClipboardFormatAvailable = user32.NewProc("IsClipboardFormatAvailable")
	procOpenClipboard              = user32.NewProc("OpenClipboard")
	procPostThreadMessageW         = user32.NewProc("PostThreadMessageW")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procRegisterClipboardFormatW   = user32.NewProc("RegisterClipboardFormatW")
	procGetModuleHandleW           = kernel32.NewProc("GetModuleHandleW")
	procGlobalLock                 = kernel32.NewProc("GlobalLock")
	procGlobalSize                 = kernel32.NewProc("GlobalSize")
	procGlobalUnlock               = kernel32.NewProc("GlobalUnlock")
)

const (
	cfBitmap      = 2
	cfDIB         = 8
	cfUnicodeText = 13
	cfHDROP       = 15
	cfDIBV5       = 17

	wmQuit            = 0x0012
	wmClipboardUpdate = 0x031D

	// hwndMessage parents a message-only window: no screen, no taskbar, only
	// a queue to deliver to.
	hwndMessage = ^uintptr(2) // (HWND)-3

	errorClassAlreadyExists = 1410
)

// openWindow is how long a busy clipboard is tried before this change is given
// up on. The writer that just changed it — the dictation tool — may still hold
// it open, and no second notification will come to try again on.
const openWindow = 250 * time.Millisecond

var className = windows.StringToUTF16Ptr("discobox-clipwatch")

// wndProc is the window's procedure. A callback cannot be freed, so there is
// one per process, however many watches it runs.
var wndProc = sync.OnceValue(func() uintptr {
	return windows.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
		return r
	})
})

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   uintptr
	icon       uintptr
	cursor     uintptr
	background uintptr
	menuName   *uint16
	className  *uint16
	iconSm     uintptr
}

type point struct{ x, y int32 }

type message struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      point
}

// Start watches the clipboard from this process.
func Start(ctx context.Context) *Watcher {
	w := NewWatcher()
	go func() {
		err := watch(ctx, func(_ uint32, text string, image bool) error {
			w.Record(text, image)
			return nil
		})
		if err != nil && ctx.Err() == nil {
			w.fail(err)
		}
	}()
	return w
}

// Serve is the helper: it reports the clipboard to out, one line per change,
// until in closes — the console that started it has gone — or out cannot be
// written.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, in)
		cancel()
	}()
	return watch(ctx, func(seq uint32, text string, image bool) error {
		_, err := io.WriteString(out, encodeLine(seq, text, image))
		return err
	})
}

// watch reports the clipboard once at the start and again on every change
// Windows notifies of, until ctx ends.
//
// The notification goes to a window, and a window's messages to the thread
// that made it, so the whole watch runs on one locked thread blocked in
// GetMessage. Ending it is a WM_QUIT posted to that thread.
func watch(ctx context.Context, report func(seq uint32, text string, image bool) error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	instance, _, _ := procGetModuleHandleW.Call(0)
	class := wndClassEx{wndProc: wndProc(), instance: instance, className: className}
	class.size = uint32(unsafe.Sizeof(class))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&class))); r == 0 {
		var errno windows.Errno
		if !errors.As(err, &errno) || errno != errorClassAlreadyExists {
			return fmt.Errorf("clipwatch: register window class: %w", err)
		}
	}
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, instance, 0)
	if hwnd == 0 {
		return fmt.Errorf("clipwatch: create window: %w", err)
	}
	defer func() { _, _, _ = procDestroyWindow.Call(hwnd) }()
	if r, _, err := procAddClipboardFormatListener.Call(hwnd); r == 0 {
		return fmt.Errorf("clipwatch: listen for clipboard changes: %w", err)
	}

	thread := windows.GetCurrentThreadId()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_, _, _ = procPostThreadMessageW.Call(uintptr(thread), wmQuit, 0, 0)
		case <-stop:
		}
	}()

	png := registeredFormat("PNG")
	// The first report is the clipboard the watch starts on, and is made even
	// when the clipboard is too busy to read: it is what every later report is
	// a change from, and without it the first change would be taken for the
	// start.
	send := func(start bool) error {
		seq, _, _ := procGetClipboardSequenceNumber.Call()
		text, image, ok := read(png)
		if !ok && !start {
			return nil
		}
		return report(uint32(seq), text, image)
	}
	if err := send(true); err != nil {
		return err
	}
	var msg message
	for {
		r, _, err := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		switch int32(r) {
		case 0: // WM_QUIT
			return nil
		case -1:
			return fmt.Errorf("clipwatch: read messages: %w", err)
		}
		if msg.message == wmClipboardUpdate {
			if err := send(false); err != nil {
				return err
			}
			continue
		}
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

// read opens the clipboard and takes its text and whether it may hold an
// image, or reports that it could not be opened. Files copied in Explorer
// (CF_HDROP) count: whether one of them is an image is the clipboard read's to
// say, and without them a copied screenshot file would never reach it.
func read(png uintptr) (text string, image bool, ok bool) {
	deadline := time.Now().Add(openWindow)
	for {
		if r, _, _ := procOpenClipboard.Call(0); r != 0 {
			break
		}
		if time.Now().After(deadline) {
			return "", false, false
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _, _, _ = procCloseClipboard.Call() }()
	for _, format := range []uintptr{cfDIB, cfDIBV5, cfBitmap, cfHDROP, png} {
		if format == 0 {
			continue
		}
		if r, _, _ := procIsClipboardFormatAvailable.Call(format); r != 0 {
			image = true
			break
		}
	}
	return unicodeText(), image, true
}

// unicodeText is the open clipboard's text, empty when it has none or more
// than MaxText.
func unicodeText() string {
	handle, _, _ := procGetClipboardData.Call(cfUnicodeText)
	if handle == 0 {
		return ""
	}
	size, _, _ := procGlobalSize.Call(handle)
	if size == 0 || size > 2*MaxText {
		return ""
	}
	ptr, _, _ := procGlobalLock.Call(handle)
	if ptr == 0 {
		return ""
	}
	defer func() { _, _, _ = procGlobalUnlock.Call(handle) }()
	// ptr is the locked global the clipboard handed back, size bytes long. It
	// is reinterpreted through its own address because it arrives as a
	// uintptr, and converting one to a pointer directly is what vet forbids.
	units := unsafe.Slice((*uint16)(*(*unsafe.Pointer)(unsafe.Pointer(&ptr))), size/2)
	return windows.UTF16ToString(units)
}

// registeredFormat is a named clipboard format's id, 0 if it cannot be had.
func registeredFormat(name string) uintptr {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0
	}
	id, _, _ := procRegisterClipboardFormatW.Call(uintptr(unsafe.Pointer(p)))
	return id
}
