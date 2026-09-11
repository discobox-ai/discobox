package store

import (
	"context"
	"regexp"

	"gorm.io/gorm"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// peerIDPattern is what may be looked up: a full endpoint ID, or a prefix of
// one. Anything else is not a name any row can have.
//
// It is enforced here and not only in the service because the prefix reaches a
// LIKE pattern below, and `%` and `_` are wildcards there. A lookup for `%`
// would otherwise match every row — and on a server with exactly one
// enrollment, resolve to it (ADR 0095 §2, enrolled iroh IDs). Validating at the edge of the store
// means no caller can reintroduce that, whatever it validated for itself.
//
// The alphabet is Crockford's, which ADR 0097 §5 names: callers normalize
// before they get here, so anything outside it — including the i, l, o and u
// that normalization folds away — is not a prefix any stored key can have. A
// lookup for something that is not a peer ID still returns "no such peer"
// rather than a parse error, because naming nothing and being malformed are
// the same answer to "which row did you mean".
var peerIDPattern = regexp.MustCompile(`^[` + endpoint.CrockfordAlphabet + `]{1,58}$`)

func (s *Store) CreatePeer(ctx context.Context, enrolled *model.Peer) error {
	write, err := s.getWrite(ctx)
	if err != nil {
		return err
	}
	return write.Create(enrolled).Error
}

// GetPeer returns one peer by its full ID or a unique prefix.
func (s *Store) GetPeer(ctx context.Context, idOrPrefix string) (*model.Peer, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	return firstPeerByPrefix(read, idOrPrefix)
}

// ListPeers returns every enrolled endpoint ID, oldest first.
func (s *Store) ListPeers(ctx context.Context) ([]model.Peer, error) {
	read, err := s.getRead(ctx)
	if err != nil {
		return nil, err
	}
	var out []model.Peer
	err = read.Order("created_at ASC").Find(&out).Error
	return out, err
}

// PeerExists reports whether a peer is enrolled. It is the admission path's
// question, so it is an exact primary-key lookup and never a prefix match:
// admitting a peer whose ID merely starts like an enrolled one would admit a
// different identity.
//
// It takes the identity rather than a string, and derives the stored form
// itself. A string parameter invited exactly one bug, and got it: admission
// passed the display form while rows are keyed on the compact one, so every
// peer enrolled through the API was refused at the gate with the API insisting
// it was enrolled. Types are what stop the two spellings meeting.
func (s *Store) PeerExists(ctx context.Context, peer endpoint.IrohID) (bool, error) {
	id := peer.Key()
	read, err := s.getRead(ctx)
	if err != nil {
		return false, err
	}
	var count int64
	if err := read.Model(&model.Peer{}).Where("id = ?", id).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// DeletePeer revokes an enrollment by its full endpoint ID or a unique
// prefix. The lookup and the delete share a transaction because the prefix has
// to resolve to the row the delete removes.
func (s *Store) DeletePeer(ctx context.Context, idOrPrefix string) error {
	return s.Transaction(ctx, func(_ *Store, tx *gorm.DB) error {
		enrolled, err := firstPeerByPrefix(tx, idOrPrefix)
		if err != nil {
			return err
		}
		return tx.Delete(enrolled).Error
	})
}

// firstPeerByPrefix resolves a full peer ID or an unambiguous prefix to one row.
//
// It does not use firstByID, which is written for generated IDs: those carry a
// `_` separator, so id.IsGenerated sends them down its exact-match branch and
// its LIKE branch is never reached with operator input. A peer ID has no
// separator and would take the LIKE branch every time.
func firstPeerByPrefix(db *gorm.DB, idOrPrefix string) (*model.Peer, error) {
	if !peerIDPattern.MatchString(idOrPrefix) {
		return nil, ErrNotFound
	}
	// A prefix has to say more than the version tag every peer ID carries.
	//
	// Without this, "d1" is a prefix of every row, so on a server with exactly
	// one enrolled peer it resolves to that peer and revokes it — the same
	// failure as the LIKE wildcard above, reached with an ordinary-looking
	// string instead of a metacharacter, and worst on a fresh server where the
	// one enrolled peer is the only way in. Revocation must never act on a row
	// the operator did not name (ADR 0095 §2, enrolled iroh IDs).
	if len(idOrPrefix) <= len(endpoint.PeerIDVersion) {
		return nil, ErrNotFound
	}
	var matches []model.Peer
	// Two is enough to tell "one match" from "ambiguous", which is all the
	// caller can act on.
	if err := db.Where("id = ? OR id LIKE ?", idOrPrefix, idOrPrefix+"%").Limit(2).Find(&matches).Error; err != nil {
		return nil, err
	}
	for i := range matches {
		if matches[i].ID == idOrPrefix {
			return &matches[i], nil
		}
	}
	if len(matches) != 1 {
		return nil, ErrNotFound
	}
	return &matches[0], nil
}
