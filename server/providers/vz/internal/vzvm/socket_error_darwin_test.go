//go:build darwin && cgo

package vzvm

import (
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
				if !errors.As(err, &native) || native != original || !strings.Contains(err.Error(), "native detail") {
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
		if got := socketError("dial", "connect", original); got != original {
			t.Fatalf("error = %v, want original %v", got, original)
		}
	}
}
