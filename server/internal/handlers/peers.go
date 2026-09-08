package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func (h *Handler) ListPeers(ctx context.Context) (serverapi.ListPeersRes, error) {
	ids, err := h.services.Peers.ListPeers(ctx)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListPeersBody](struct {
		Peers any `json:"peers"`
	}{Peers: ids})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) CreatePeer(ctx context.Context, req *apimodel.CreatePeerBody) (serverapi.CreatePeerRes, error) {
	enrolled, err := h.services.Peers.CreatePeer(ctx, *req)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.Peer](enrolled)
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) DeletePeer(ctx context.Context, params serverapi.DeletePeerParams) (serverapi.DeletePeerRes, error) {
	if err := h.services.Peers.DeletePeer(ctx, params.PeerId); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeletePeerNoContent{}, nil
}
