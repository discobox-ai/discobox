package cli

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
)

func TestAttachDoneErrors(t *testing.T) {
	for _, err := range []error{
		nil,
		io.EOF,
		net.ErrClosed,
		os.ErrClosed,
		errors.New("write: broken pipe"),
		errors.New("read tcp: use of closed network connection"),
		errors.New("connection reset by peer"),
	} {
		if !isAttachDone(err) {
			t.Fatalf("isAttachDone(%v) = false, want true", err)
		}
	}
	if isAttachDone(errors.New("permission denied")) {
		t.Fatal("permission denied was classified as attach done")
	}
}
