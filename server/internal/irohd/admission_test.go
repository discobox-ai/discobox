package irohd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

type fakePeerStore struct {
	enrolled map[endpoint.IrohID]bool
	err      error
}

func (f fakePeerStore) PeerExists(_ context.Context, peer endpoint.IrohID) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.enrolled[peer], nil
}

func testEndpointID(t *testing.T, first byte) endpoint.IrohID {
	t.Helper()
	var id endpoint.IrohID
	id[0] = first
	return id
}

// The file layer answers before the store exists at all, which is what makes it
// the way back in (ADR 0095 §3).
func TestAdmissionAdmitsFromTheFileBeforeTheStoreArrives(t *testing.T) {
	dataDir := t.TempDir()
	id := testEndpointID(t, 0xaa)
	writeAuthorizedIDs(t, dataDir, "# an operator's own machine\n"+id.String()+" laptop\n")

	admission := NewAdmission(dataDir)
	if err := admission.Authorize(context.Background(), id); err != nil {
		t.Fatalf("a file-layer ID was refused before the store arrived: %v", err)
	}
}

func TestAdmissionAdmitsAnEnrolledID(t *testing.T) {
	id := testEndpointID(t, 0xbb)
	admission := NewAdmission(t.TempDir())
	admission.SetStore(fakePeerStore{enrolled: map[endpoint.IrohID]bool{id: true}})

	if err := admission.Authorize(context.Background(), id); err != nil {
		t.Fatalf("an enrolled ID was refused: %v", err)
	}
}

func TestAdmissionRefusesAnIDInNeitherLayer(t *testing.T) {
	id := testEndpointID(t, 0xcc)
	admission := NewAdmission(t.TempDir())
	admission.SetStore(fakePeerStore{})

	err := admission.Authorize(context.Background(), id)
	if err == nil {
		t.Fatal("an unenrolled ID was admitted")
	}
	if !strings.Contains(err.Error(), "not authorized") {
		t.Fatalf("refusal = %q, want it to name authorization", err)
	}
}

// A store that cannot be read admits nobody it has not already admitted from
// the file. Failing open here would be an unauthenticated control plane.
func TestAdmissionFailsClosedWhenTheStoreErrors(t *testing.T) {
	admission := NewAdmission(t.TempDir())
	admission.SetStore(fakePeerStore{err: errors.New("database is gone")})

	if err := admission.Authorize(context.Background(), testEndpointID(t, 0xdd)); err == nil {
		t.Fatal("an unreadable store admitted a peer")
	}
}

// The decision in ADR 0095 §4: a peer arriving before the database is open
// waits for it rather than being told it is not enrolled.
func TestAdmissionWaitsForTheStore(t *testing.T) {
	id := testEndpointID(t, 0xee)
	admission := NewAdmission(t.TempDir())

	decided := make(chan error, 1)
	go func() { decided <- admission.Authorize(context.Background(), id) }()

	select {
	case err := <-decided:
		t.Fatalf("admission answered %v before the store arrived; it must wait", err)
	case <-time.After(50 * time.Millisecond):
	}

	admission.SetStore(fakePeerStore{enrolled: map[endpoint.IrohID]bool{id: true}})

	select {
	case err := <-decided:
		if err != nil {
			t.Fatalf("a waiting enrolled peer was refused: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("installing the store did not release the waiting admission check")
	}
}

// When startup fails the listener is torn down, canceling the context. A
// waiter must be answered by that rather than dying with the process.
func TestAdmissionCancellationRefusesWaiters(t *testing.T) {
	admission := NewAdmission(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())

	decided := make(chan error, 1)
	go func() { decided <- admission.Authorize(ctx, testEndpointID(t, 0xff)) }()

	select {
	case err := <-decided:
		t.Fatalf("admission answered %v before cancellation", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-decided:
		if err == nil {
			t.Fatal("a canceled admission check admitted the peer")
		}
		// The peer reads this. "Shutting down" and "not enrolled" send an
		// operator to different places (ADR 0095 §4).
		if !strings.Contains(err.Error(), "shutting down") {
			t.Fatalf("refusal = %q, want it to say the server is going away", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancellation did not release the waiting admission check")
	}
}
