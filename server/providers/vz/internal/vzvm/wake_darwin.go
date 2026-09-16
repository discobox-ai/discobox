//go:build darwin && cgo

package vzvm

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <IOKit/pwr_mgt/IOPMLib.h>
#include <IOKit/IOMessage.h>
#include <stdlib.h>
#include <unistd.h>

typedef struct {
	IONotificationPortRef port;
	io_object_t notifier;
	io_connect_t root;
	CFRunLoopRef loop;
	int stopping;
	int wake_fd;
} discobox_wake_observer;

static void discobox_power_changed(void *ref, io_service_t service,
		natural_t message, void *argument) {
	(void)service;
	discobox_wake_observer *observer = ref;
	switch (message) {
	case kIOMessageCanSystemSleep:
	case kIOMessageSystemWillSleep:
		IOAllowPowerChange(observer->root, (long)argument);
		break;
	case kIOMessageSystemHasPoweredOn:
		// The pipe is nonblocking. One pending wake is enough to resync all VMs.
		(void)write(observer->wake_fd, "w", 1);
		break;
	}
}

static discobox_wake_observer *discobox_observe_wake(int wake_fd) {
	discobox_wake_observer *observer = calloc(1, sizeof(*observer));
	if (observer == NULL) return NULL;
	observer->wake_fd = wake_fd;
	observer->root = IORegisterForSystemPower(observer, &observer->port,
		discobox_power_changed, &observer->notifier);
	if (observer->root == 0) {
		free(observer);
		return NULL;
	}
	observer->loop = CFRunLoopGetCurrent();
	CFRetain(observer->loop);
	CFRunLoopAddSource(observer->loop,
		IONotificationPortGetRunLoopSource(observer->port), kCFRunLoopCommonModes);
	return observer;
}

static void discobox_run_wake_observer(discobox_wake_observer *observer) {
	while (!__atomic_load_n(&observer->stopping, __ATOMIC_SEQ_CST)) {
		CFRunLoopRunInMode(kCFRunLoopDefaultMode, 1.0, 0);
	}
	CFRunLoopRemoveSource(observer->loop,
		IONotificationPortGetRunLoopSource(observer->port), kCFRunLoopCommonModes);
	IODeregisterForSystemPower(&observer->notifier);
	IOServiceClose(observer->root);
	IONotificationPortDestroy(observer->port);
}

static void discobox_stop_wake_observer(discobox_wake_observer *observer) {
	__atomic_store_n(&observer->stopping, 1, __ATOMIC_SEQ_CST);
	CFRunLoopStop(observer->loop);
	CFRunLoopWakeUp(observer->loop);
}

static void discobox_free_wake_observer(discobox_wake_observer *observer) {
	CFRelease(observer->loop);
	free(observer);
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
)

// WakeMonitor reports system wake, including a laptop waking with no GUI app
// running. IOKit registration is observation only; it does not prevent sleep.
type WakeMonitor struct {
	events   chan struct{}
	read     *os.File
	write    *os.File
	observer *C.discobox_wake_observer
	done     chan struct{}
	once     sync.Once
}

func NewWakeMonitor() (*WakeMonitor, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create wake pipe: %w", err)
	}
	if err := syscall.SetNonblock(int(write.Fd()), true); err != nil {
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("make wake pipe nonblocking: %w", err)
	}
	m := &WakeMonitor{
		events: make(chan struct{}, 1), read: read, write: write,
		done: make(chan struct{}),
	}
	ready := make(chan *C.discobox_wake_observer, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		observer := C.discobox_observe_wake(C.int(write.Fd()))
		ready <- observer
		if observer != nil {
			C.discobox_run_wake_observer(observer)
		}
		close(m.done)
	}()
	if m.observer = <-ready; m.observer == nil {
		<-m.done
		_ = read.Close()
		_ = write.Close()
		return nil, fmt.Errorf("register macOS system wake notification")
	}
	go func() {
		defer close(m.events)
		var buf [32]byte
		for {
			if _, err := read.Read(buf[:]); err != nil {
				return
			}
			select {
			case m.events <- struct{}{}:
			default:
			}
		}
	}()
	return m, nil
}

func (m *WakeMonitor) Events() <-chan struct{} { return m.events }

func (m *WakeMonitor) Close() {
	m.once.Do(func() {
		C.discobox_stop_wake_observer(m.observer)
		<-m.done
		C.discobox_free_wake_observer(m.observer)
		_ = m.write.Close()
		_ = m.read.Close()
	})
}
