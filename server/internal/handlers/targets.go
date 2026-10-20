package handlers

import (
	"context"

	serverapi "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/targetdb"
)

// The target database (ADR 26-10-20-782 §6). The routes are a person's: the
// sandbox role lists none of them, and the service refuses a discobox that
// reached it another way.

func (h *Handler) ListProjectTargets(ctx context.Context, params serverapi.ListProjectTargetsParams) (serverapi.ListProjectTargetsRes, error) {
	entries, err := h.services.Targets.ListProjectTargets(ctx, params.ProjectId)
	if err != nil {
		return apiError(err), nil
	}
	body, err := services.Convert[apimodel.ListProjectTargetsBody](struct {
		Targets []model.TargetEntry `json:"targets"`
	}{Targets: entries})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func (h *Handler) GetProjectTarget(ctx context.Context, params serverapi.GetProjectTargetParams) (serverapi.GetProjectTargetRes, error) {
	entry, err := h.services.Targets.GetProjectTarget(ctx, params.ProjectId, targetdb.NormalizeHost(params.Host))
	if err != nil {
		return apiError(err), nil
	}
	return targetEntryBody(entry)
}

func (h *Handler) PutProjectTarget(ctx context.Context, req *apimodel.PutProjectTargetBody, params serverapi.PutProjectTargetParams) (serverapi.PutProjectTargetRes, error) {
	entry, err := h.services.Targets.PutProjectTarget(ctx, params.ProjectId, params.Host, *req)
	if err != nil {
		return apiError(err), nil
	}
	return targetEntryBody(entry)
}

func (h *Handler) DeleteProjectTarget(ctx context.Context, params serverapi.DeleteProjectTargetParams) (serverapi.DeleteProjectTargetRes, error) {
	if err := h.services.Targets.DeleteProjectTarget(ctx, params.ProjectId, targetdb.NormalizeHost(params.Host)); err != nil {
		return apiError(err), nil
	}
	return &serverapi.DeleteProjectTargetNoContent{}, nil
}

func (h *Handler) GetProjectTargetDocument(ctx context.Context, params serverapi.GetProjectTargetDocumentParams) (serverapi.GetProjectTargetDocumentRes, error) {
	text, err := h.services.Targets.GetProjectTargetDocument(ctx, params.ProjectId, targetdb.NormalizeHost(params.Host), params.DocumentName)
	if err != nil {
		return apiError(err), nil
	}
	// The encoder closes the text once it is written.
	return &serverapi.GetProjectTargetDocumentOK{Data: text}, nil
}

func (h *Handler) LookupProjectTarget(ctx context.Context, req *apimodel.LookupProjectTargetBody, params serverapi.LookupProjectTargetParams) (serverapi.LookupProjectTargetRes, error) {
	statement, err := services.PolicyStatementFromAPI(req.Statement)
	if err != nil {
		return apiError(err), nil
	}
	entry, lookup, err := h.services.Targets.LookupProjectTarget(ctx, params.ProjectId, req.Host, statement)
	if err != nil {
		return apiError(err), nil
	}
	// The documents are what to read for an action with no known shape.
	documents := []targetdb.Document{}
	if !lookup.Known() {
		documents = entry.Documents
	}
	body, err := services.Convert[apimodel.TargetLookup](struct {
		Host      string              `json:"host"`
		Version   string              `json:"version"`
		Matches   []targetdb.Match    `json:"matches"`
		Missing   []string            `json:"missing"`
		Documents []targetdb.Document `json:"documents"`
	}{entry.Host, entry.Version, lookup.Matches, lookup.Missing, documents})
	if err != nil {
		return nil, err
	}
	return &body, nil
}

func targetEntryBody(entry *model.TargetEntry) (*apimodel.TargetEntry, error) {
	body, err := services.Convert[apimodel.TargetEntry](entry)
	if err != nil {
		return nil, err
	}
	return &body, nil
}
