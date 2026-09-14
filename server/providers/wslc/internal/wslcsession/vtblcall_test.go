//go:build windows

package wslcsession

import (
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

// fakeCOMObject is the memory layout of a COM object as vtblCall reads it: a
// pointer to a table of function pointers. Slot 0 writes a known value through
// the out-parameter it is given, the way QueryInterface, CreateSession and
// every other out-parameter method does.
type fakeCOMObject struct {
	vtbl *[1]uintptr
}

var (
	fakeOutMarker     = new(byte)
	fakeWriteOut      uintptr
	fakeWriteOutSetup sync.Once
)

func newFakeCOMObject() unsafe.Pointer {
	fakeWriteOutSetup.Do(func() {
		fakeWriteOut = syscall.NewCallback(func(this uintptr, out *unsafe.Pointer) uintptr {
			*out = unsafe.Pointer(fakeOutMarker)
			return 0
		})
	})
	obj := &fakeCOMObject{vtbl: &[1]uintptr{fakeWriteOut}}
	return unsafe.Pointer(obj)
}

// consumeStack burns stack in frames of a fixed size before making the call,
// so that across a sweep of depths the goroutine's stack runs out - and is
// copied somewhere larger - at every point between taking &out and the
// syscall that writes through it.
//
//go:noinline
func consumeStack(depth int, call func() bool) bool {
	var pad [128]byte
	_ = pad
	if depth == 0 {
		return call()
	}
	return consumeStack(depth-1, call)
}

// An out-parameter written by a COM method has to land in the variable its
// address was taken from, whatever the goroutine's stack does in between.
//
// vtblCall takes its arguments as uintptr, so `uintptr(unsafe.Pointer(&out))`
// is converted by the caller, before vtblCall runs. Without
// //go:uintptrescapes on vtblCall the compiler keeps out on the stack, and a
// stack that grows at vtblCall's entry is copied elsewhere: the method then
// writes into the old copy, out keeps its zero value, and the call reports
// success. That is the nil process pointer that crashed the server in
// GetStdHandle - and the write lands in freed stack memory besides.
func TestVTableCallOutParametersSurviveStackGrowth(t *testing.T) {
	obj := newFakeCOMObject()
	for depth := 0; depth < 400; depth++ {
		results := make(chan bool, 1)
		go func() {
			results <- consumeStack(depth, func() bool {
				var out unsafe.Pointer
				vtblCall(obj, 0, uintptr(unsafe.Pointer(&out)))
				return out == unsafe.Pointer(fakeOutMarker)
			})
		}()
		if !<-results {
			t.Fatalf("at stack depth %d the method's write through &out did not reach out", depth)
		}
	}
}
