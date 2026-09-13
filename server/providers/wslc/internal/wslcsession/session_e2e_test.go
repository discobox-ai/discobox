//go:build windows

package wslcsession

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSessionCompatCallsE2E covers the two SDK-facing calls the provider's own
// e2e tests never reach: CreateVolume, which no driver asks for yet, and
// Terminate, whose HRESULT is discarded on the teardown path. Both sit at
// vtable slots derived from an IDL rather than observed, and a wrong slot is a
// call to the neighbouring method, so nothing short of running them says they
// are right.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestSessionCompatCallsE2E -v ./providers/wslc/internal/wslcsession/
func TestSessionCompatCallsE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	const (
		displayName = "discobox-wslcsession-e2e"
		volumeName  = "wslcsession-e2e-vol"
	)
	storage := filepath.Join(t.TempDir(), "vm")

	session, err := NewSession(Options{
		DisplayName:      displayName,
		StoragePath:      storage,
		MaxStorageSizeMB: 8192,
		Volumes:          []VolumeOptions{{Name: volumeName, SizeMB: 64}},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer func() { _ = session.Close() }()

	// A volume the guest's own dockerd reports back is the only proof that
	// CreateVolume reached CreateVolume, and that the .vhdx behind it was
	// really created.
	inspect := guestOutput(t, session, "docker volume inspect "+volumeName+" 2>&1")
	if !strings.Contains(inspect, `"Name": "`+volumeName+`"`) {
		t.Fatalf("dockerd does not know volume %q; it answered %q", volumeName, inspect)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// wslc refuses a duplicate display name while the session holding it is
	// alive, so claiming the name again is what shows Terminate ended the VM
	// rather than returning a quiet failure. The retry is for the service's own
	// teardown, which finishes shortly after Close returns; what is being
	// asserted is that the VM does not outlive Close, not how promptly the name
	// comes back.
	deadline := time.Now().Add(30 * time.Second)
	for {
		second, err := NewSession(Options{
			DisplayName:      displayName,
			StoragePath:      storage,
			MaxStorageSizeMB: 8192,
		})
		if err == nil {
			_ = second.Close()
			return
		}
		if !errors.Is(err, ErrSessionExists) {
			t.Fatalf("second NewSession: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the closed session's VM is still holding %q: %v", displayName, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// guestOutput runs one shell command in the guest and returns everything it
// wrote to stdout.
//
// The guest ships a docker CLI of its own, which is what makes this the whole
// of the machinery needed to ask dockerd a question: this package can start a
// guest process and nothing else, by design, so a test that needed to speak the
// Engine API over a socket would have to bring a guest-side program with it.
func guestOutput(t *testing.T, session *Session, command string) string {
	t.Helper()

	conn, err := session.StartProcess("/bin/sh", []string{"/bin/sh", "-c", command})
	if err != nil {
		t.Fatalf("StartProcess(%q): %v", command, err)
	}
	defer func() { _ = conn.Close() }()

	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read output of %q: %v", command, err)
	}
	return string(out)
}
