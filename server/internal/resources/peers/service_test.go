package peers_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	apigen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/resources/peers"
	"github.com/discobox-ai/discobox/server/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close db: %v", err)
		}
	})
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return store.New(db.Write, db.Read)
}

// A real peer ID, in the display form an operator pastes and the compact form
// it is stored as.
var (
	testPeer   = testPeerID()
	displayID  = testPeer.String()
	compactID  = testPeer.Key()
	shoutingID = strings.ToUpper(displayID)
)

func testPeerID() endpoint.IrohID {
	var key [32]byte
	key[0] = 0xaa
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		panic(err)
	}
	return id
}

func statusOf(t *testing.T, err error) int {
	t.Helper()
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		t.Fatalf("error %v carries no HTTP status", err)
	}
	return status.StatusCode()
}

// Enrolling parses strictly: this value is the address peers are admitted by,
// and a truncated ID is a different identity rather than a prefix of this one.
// Prefixes are for naming a row, which is what revocation does.
func TestCreateRejectsAnythingThatIsNotAPeerID(t *testing.T) {
	svc := peers.NewService(newStore(t))

	for _, bad := range []string{"", "   ", "d1", "not-a-peer", compactID + "zz", "%",
		"aa11223344556677889900aabbccddeeff00112233445566778899aabbccddee"} {
		_, err := svc.CreatePeer(context.Background(), apimodel.CreatePeerBody{PeerId: bad})
		if err == nil {
			t.Fatalf("CreatePeer(%q) succeeded, want a rejection", bad)
		}
		if got := statusOf(t, err); got != http.StatusBadRequest {
			t.Fatalf("CreatePeer(%q) status = %d, want 400", bad, got)
		}
	}
}

// The stored ID is the one an operator will compare against authorized_ids and
// against what `discobox admin peer id` printed, so it is normalized once here
// rather than at every point of comparison — dashes and case wash out.
func TestCreateNormalizesThePeerID(t *testing.T) {
	store := newStore(t)
	svc := peers.NewService(store)

	enrolled, err := svc.CreatePeer(context.Background(), apimodel.CreatePeerBody{
		PeerId: "  " + shoutingID + "  ",
		Name:   apigen.NewOptString(" laptop "),
	})
	if err != nil {
		t.Fatalf("CreatePeer() error = %v", err)
	}
	// What comes back is the one written form, whatever was sent in.
	if enrolled.ID != displayID {
		t.Fatalf("returned ID = %q, want the written form %q", enrolled.ID, displayID)
	}
	if enrolled.Name != "laptop" {
		t.Fatalf("stored name = %q, want %q", enrolled.Name, "laptop")
	}

	// Storage keys on the compact form, so a prefix match is a prefix of one
	// stable string rather than of whatever hyphenation somebody pasted.
	row, err := store.GetPeer(context.Background(), compactID)
	if err != nil {
		t.Fatalf("GetPeer(compact) error = %v", err)
	}
	if row.ID != compactID {
		t.Fatalf("row ID = %q, want the compact form %q", row.ID, compactID)
	}
}

// Enrolling twice is a conflict rather than a silent second row: two rows for
// one identity would make revoking it look like it worked while the other
// still admits the peer.
func TestCreateRefusesADuplicate(t *testing.T) {
	svc := peers.NewService(newStore(t))
	ctx := context.Background()

	if _, err := svc.CreatePeer(ctx, apimodel.CreatePeerBody{PeerId: displayID}); err != nil {
		t.Fatalf("first CreatePeer() error = %v", err)
	}
	_, err := svc.CreatePeer(ctx, apimodel.CreatePeerBody{PeerId: shoutingID})
	if err == nil {
		t.Fatal("enrolling the same ID twice succeeded")
	}
	if got := statusOf(t, err); got != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", got)
	}
}

func TestDeleteReportsWhatItCouldNotFind(t *testing.T) {
	svc := peers.NewService(newStore(t))

	err := svc.DeletePeer(context.Background(), "d1zzzzz")
	if err == nil {
		t.Fatal("revoking an ID that is not enrolled succeeded")
	}
	if got := statusOf(t, err); got != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", got)
	}
	if !strings.Contains(err.Error(), "d1zzzzz") {
		t.Fatalf("error %q does not name what was looked up", err)
	}
}
