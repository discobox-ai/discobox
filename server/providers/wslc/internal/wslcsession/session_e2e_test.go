//go:build windows

package wslcsession

import (
	"bufio"
	"errors"
	"io"
	"net/http"
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
	body := dockerGet(t, session, "/volumes/"+volumeName)
	if !strings.Contains(body, `"Name":"`+volumeName+`"`) {
		t.Fatalf("dockerd does not know volume %q; it answered %q", volumeName, body)
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

// dockerGet performs one HTTP/1.0 request against the guest's dockerd over the
// session's own relay and returns the response body. HTTP/1.0 with no
// keep-alive means dockerd answers and closes, so no client machinery is
// needed to know where the response ends.
func dockerGet(t *testing.T, session *Session, path string) string {
	t.Helper()

	conn, err := session.DockerConn()
	if err != nil {
		t.Fatalf("DockerConn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if _, err := io.WriteString(conn, "GET "+path+" HTTP/1.0\r\n\r\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("drain body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %s: %s", path, resp.Status, body)
	}
	return string(body)
}
