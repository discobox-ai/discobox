package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Two peer IDs whose stored forms differ from the first symbol, so a prefix of
// one names only that one.
var (
	testPeerA = testPeerKey(0xaa)
	testPeerB = testPeerKey(0xbb)
)

func testPeerKey(first byte) string { return testPeer(first).Key() }

func testPeer(first byte) endpoint.IrohID {
	var key [32]byte
	key[0] = first
	id, err := endpoint.IrohIDFromPublicKey(key[:])
	if err != nil {
		panic(err)
	}
	return id
}

func enrollIrohIDs(t *testing.T, ids ...string) *store.Store {
	t.Helper()
	s := newTestStore(t)
	for _, id := range ids {
		if err := s.CreatePeer(context.Background(), &model.Peer{ID: id}); err != nil {
			t.Fatalf("CreatePeer(%s) error = %v", id, err)
		}
	}
	return s
}

// A LIKE wildcard in the lookup value must not resolve to a row.
//
// firstByID's non-generated branch builds `id LIKE value || '%'`, and a peer
// ID has no `_` separator, so it takes that branch every time. Without the
// charset check, `%` matches every row — and on a server with exactly one
// enrollment the "unambiguous prefix" rule would then revoke a credential the
// caller never named (ADR 0095 §2).
func TestGetPeerRejectsLikeWildcards(t *testing.T) {
	s := enrollIrohIDs(t, testPeerA)

	for _, lookup := range []string{"%", "_", "aa%", "a_", "%%", strings.Repeat("_", 64)} {
		if _, err := s.GetPeer(context.Background(), lookup); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetPeer(%q) error = %v, want store.ErrNotFound", lookup, err)
		}
	}
	if err := s.DeletePeer(context.Background(), "%"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("DeletePeer(%q) error = %v, want store.ErrNotFound", "%", err)
	}
	if _, err := s.GetPeer(context.Background(), testPeerA); err != nil {
		t.Fatalf("the enrollment was removed by a wildcard lookup: %v", err)
	}
}

func TestGetPeerResolvesFullIDAndUniquePrefix(t *testing.T) {
	s := enrollIrohIDs(t, testPeerA, testPeerB)

	for _, lookup := range []string{testPeerA, testPeerA[:4], testPeerA[:12]} {
		got, err := s.GetPeer(context.Background(), lookup)
		if err != nil {
			t.Fatalf("GetPeer(%q) error = %v", lookup, err)
		}
		if got.ID != testPeerA {
			t.Fatalf("GetPeer(%q) = %s, want %s", lookup, got.ID, testPeerA)
		}
	}

	// Shared by both enrollments, so it names neither.
	if _, err := s.GetPeer(context.Background(), "d1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an ambiguous prefix resolved: %v", err)
	}
	if _, err := s.GetPeer(context.Background(), testPeerKey(0xcc)[:12]); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unmatched prefix resolved: %v", err)
	}
}

// Admission asks about an identity, and gets an answer that does not depend on
// how that identity happens to be spelled.
//
// This is the bug that shipped past three test suites: rows are keyed on the
// compact form, admission held the display form, and every peer enrolled
// through the API was refused at the gate. Taking the identity rather than a
// string is what makes the two unable to differ.
func TestPeerExistsAnswersForTheIdentity(t *testing.T) {
	peerA, peerB := testPeer(0xaa), testPeer(0xbb)
	s := enrollIrohIDs(t, peerA.Key())

	exists, err := s.PeerExists(context.Background(), peerA)
	if err != nil {
		t.Fatalf("PeerExists() error = %v", err)
	}
	if !exists {
		t.Fatal("an enrolled peer reported as not enrolled")
	}
	if exists, err = s.PeerExists(context.Background(), peerB); err != nil || exists {
		t.Fatalf("PeerExists(other) = %v, %v; want false", exists, err)
	}
}

func TestDeletePeerRemovesOnlyTheNamedRow(t *testing.T) {
	s := enrollIrohIDs(t, testPeerA, testPeerB)

	if err := s.DeletePeer(context.Background(), uniquePrefix(testPeerA, testPeerB)); err != nil {
		t.Fatalf("DeletePeer() error = %v", err)
	}
	remaining, err := s.ListPeers(context.Background())
	if err != nil {
		t.Fatalf("ListPeers() error = %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != testPeerB {
		t.Fatalf("remaining = %v, want only %s", remaining, testPeerB)
	}
	if err := s.DeletePeer(context.Background(), uniquePrefix(testPeerA, testPeerB)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete error = %v, want store.ErrNotFound", err)
	}
}

// uniquePrefix returns the shortest prefix of a that b does not share.
func uniquePrefix(a, b string) string {
	for i := 1; i <= len(a); i++ {
		if i > len(b) || a[:i] != b[:i] {
			return a[:i]
		}
	}
	panic("ids are not distinct")
}

// The version tag is a prefix of every peer ID, so it names none of them.
//
// The dangerous case is a server with exactly one enrolled peer: "d1" matches
// that one row, the "unambiguous prefix" rule resolves it, and revoking it
// takes away the only enrolled way in. Found by running `discobox admin peer
// rm d1` against a real server, not by a test.
func TestVersionPrefixAloneNamesNoPeer(t *testing.T) {
	s := enrollIrohIDs(t, testPeerA)

	for _, lookup := range []string{"d", "d1", "D1", "d1-"} {
		if _, err := s.GetPeer(context.Background(), lookup); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetPeer(%q) error = %v, want store.ErrNotFound", lookup, err)
		}
		if err := s.DeletePeer(context.Background(), lookup); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("DeletePeer(%q) error = %v, want store.ErrNotFound", lookup, err)
		}
	}
	if _, err := s.GetPeer(context.Background(), testPeerA); err != nil {
		t.Fatalf("the enrollment was revoked by a lookup that named nobody: %v", err)
	}

	// One symbol past the version is a real prefix and still resolves.
	if _, err := s.GetPeer(context.Background(), testPeerA[:len("d1")+1]); err != nil {
		t.Fatalf("a prefix one symbol past the version did not resolve: %v", err)
	}
}

// A lookup and an enrollment must reach the same row from the same typing.
//
// Crockford folds i/l to 1 and o to 0, so an operator who read a 1 as an l
// enrolls the right peer; revoking with the same string has to find it, or the
// two halves of the same identity diverge on exactly the input the alphabet
// exists to absorb (ADR 0097 §5).
func TestLookupFoldsTheSameConfusablesAsEnrollment(t *testing.T) {
	peer := testPeer(0xaa)
	s := enrollIrohIDs(t, peer.Key())

	typed := strings.NewReplacer("1", "l", "0", "O").Replace(peer.String())
	if typed == peer.String() {
		t.Skip("this peer ID has no confusable characters to mistype")
	}
	found, err := s.GetPeer(context.Background(), endpoint.NormalizePeerID(typed))
	if err != nil {
		t.Fatalf("GetPeer(%q) error = %v; the same typing enrolled it", typed, err)
	}
	if found.ID != peer.Key() {
		t.Fatalf("resolved %q, want %q", found.ID, peer.Key())
	}
}
