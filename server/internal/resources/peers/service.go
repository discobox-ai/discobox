// Package peers implements the enrolled peer resource (ADR 0095, enrolled iroh
// IDs): a machine permitted to connect to this server, managed through the API,
// beside the server-wide authorized_ids file that is not.
package peers

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/discobox-ai/discobox/endpoint"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

type Service struct {
	store *store.Store
}

func NewService(store *store.Store) *Service {
	return &Service{store: store}
}

func (s *Service) ListPeers(ctx context.Context) ([]model.Peer, error) {
	peers, err := s.store.ListPeers(ctx)
	if err != nil {
		return nil, err
	}
	for i := range peers {
		peers[i] = display(peers[i])
	}
	return peers, nil
}

func (s *Service) CreatePeer(ctx context.Context, input services.CreatePeerBody) (*model.Peer, error) {
	// Enrolling parses strictly, because this value is an address that peers
	// will be admitted by: a truncated ID is a different identity, not a
	// prefix of this one, and a mistyped one fails its check symbol here
	// rather than becoming an enrollment that admits nobody (ADR 0097 §1).
	// Lookup is the operation that accepts a prefix; enrollment is not.
	parsed, err := endpoint.ParseIrohID(input.PeerId)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	id := parsed.Key()
	existing, err := s.store.PeerExists(ctx, parsed)
	if err != nil {
		return nil, err
	}
	if existing {
		return nil, apperrors.NewStatusError(http.StatusConflict, "peer "+parsed.String()+" is already enrolled")
	}
	enrolled := &model.Peer{ID: id, Name: strings.TrimSpace(input.Name.Or(""))}
	if err := s.store.CreatePeer(ctx, enrolled); err != nil {
		return nil, err
	}
	stored, err := s.store.GetPeer(ctx, id)
	if err != nil {
		return nil, err
	}
	out := display(*stored)
	return &out, nil
}

func (s *Service) DeletePeer(ctx context.Context, idOrPrefix string) error {
	if err := s.store.DeletePeer(ctx, endpoint.NormalizePeerID(idOrPrefix)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return apperrors.NewStatusError(http.StatusNotFound, "no enrolled peer matches "+idOrPrefix)
		}
		return err
	}
	return nil
}

// display renders a stored peer for a reader.
//
// The database keys on the compact form so a prefix match is a prefix of one
// stable string, but that is storage: everywhere a peer ID is read — the API,
// the CLI, a log line, an authorized_ids file — it is the one written form,
// dashes and all (ADR 0097 §5). Letting the stored spelling out would mean an
// operator comparing `discobox admin peer id` against `peer ls` sees two
// strings and has to know they are the same identity.
func display(peer model.Peer) model.Peer {
	id, err := endpoint.ParseIrohID(peer.ID)
	if err != nil {
		// A row that is not a peer ID cannot be rendered as one. Returning it
		// unchanged shows an operator what is actually in the table, which is
		// what they need to fix it.
		return peer
	}
	peer.ID = id.String()
	return peer
}
