//go:build darwin && cgo

package vzvm

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/Code-Hex/vz/v3"
)

func TestSocketErrorExposesPOSIXFailures(t *testing.T) {
	for _, tc := range []struct {
		code  int
		errno syscall.Errno
	}{
		{54, syscall.ECONNRESET}, {61, syscall.ECONNREFUSED},
	} {
		for _, op := range []string{"dial", "listen"} {
			t.Run(fmt.Sprintf("%s/%d", op, tc.code), func(t *testing.T) {
				original := &vz.NSError{Domain: "NSPOSIXErrorDomain", Code: tc.code, LocalizedDescription: "native detail"}
				err := socketError(op, "connect", fmt.Errorf("wrapped: %w", original))
				var network *net.OpError
				if !errors.Is(err, tc.errno) || !errors.As(err, &network) || network.Op != op || network.Net != "vsock" {
					t.Fatalf("network error = %v, want %s/%v", err, op, tc.errno)
				}
				var native *vz.NSError
				if !errors.As(err, &native) || !errors.Is(native, original) || !strings.Contains(err.Error(), "native detail") {
					t.Fatalf("native error lost: %v", err)
				}
			})
		}
	}
}

func TestSocketErrorPreservesOtherFailures(t *testing.T) {
	for _, original := range []error{
		&vz.NSError{Domain: "VZErrorDomain", Code: 54},
		errors.New("plain failure"),
	} {
		if got := socketError("dial", "connect", original); !errors.Is(got, original) {
			t.Fatalf("error = %v, want original %v", got, original)
		}
	}
}

func TestConnectErrorReportsTheBoundAsAFailedDial(t *testing.T) {
	err := connectError(context.DeadlineExceeded, nil, false)
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		t.Fatalf("connectError = %v, want a dial net.OpError", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("connectError = %v reads as the caller's deadline, which has not passed", err)
	}
	if opErr.Timeout() {
		t.Fatalf("connectError = %v reports a timeout, which callers read as a request the guest may still be working on", err)
	}
}

func TestConnectErrorKeepsTheCallersOwnEnding(t *testing.T) {
	for _, callerErr := range []error{context.Canceled, context.DeadlineExceeded} {
		if err := connectError(callerErr, callerErr, false); !errors.Is(err, callerErr) {
			t.Fatalf("connectError = %v, want the caller's %v", err, callerErr)
		}
	}
}

func TestConnectErrorSaysTheVMWasClosed(t *testing.T) {
	if err := connectError(context.Canceled, nil, true); !errors.Is(err, errVMClosed) {
		t.Fatalf("connectError = %v, want errVMClosed", err)
	}
}
