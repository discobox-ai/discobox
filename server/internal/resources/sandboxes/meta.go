package sandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/sandboxmeta"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandboxagentclient"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

// UpdateSandboxMeta carries a change to the sandbox's description or tags into
// the sandbox, which applies it to its meta file, and records what the sandbox
// then holds (ADR 0136). The sandbox is the system of record: a change it did
// not take is an error, never a change recorded here alone, because the next
// status report would quietly undo it.
//
// Writing a file into the sandbox is gated as exec:write, the scope that
// already allows any such write.
func (s *Service) UpdateSandboxMeta(ctx context.Context, projectID, sandboxID string, input services.UpdateSandboxMetaBody) (*model.Sandbox, error) {
	change := sandboxmeta.Change{RemoveTags: input.RemoveTags}
	if description, ok := input.Description.Get(); ok {
		change.Description = &description
	}
	change.SetTags, _ = input.SetTags.Get()
	// Checked here as well as in the sandbox, so a change that cannot be taken
	// is refused without starting a stopped sandbox to be told so.
	if err := change.Check(); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadRequest, err.Error())
	}
	lease, sandboxModel, err := s.AcquireSandboxHTTPClient(ctx, projectID, sandboxID, []string{poolagentauth.ScopeExecWrite})
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	target, err := sandboxagentclient.TargetURL(lease.BaseURL, sandboxModel.ProjectID, sandboxModel.PoolID, sandboxModel.ID, "/meta")
	if err != nil {
		return nil, err
	}
	request := sandboxapi.UpdateSandboxMetaBody{RemoveTags: change.RemoveTags}
	if change.Description != nil {
		request.Description = sandboxapi.NewOptString(*change.Description)
	}
	if change.SetTags != nil {
		request.SetTags = sandboxapi.NewOptUpdateSandboxMetaBodySetTags(sandboxapi.UpdateSandboxMetaBodySetTags(change.SetTags))
	}
	body, err := json.Marshal(&request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := sandboxagentclient.HTTPClient(lease).Do(req)
	if err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadGateway, fmt.Sprintf("the sandbox could not be reached to change its meta: %v", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, sandboxMetaError(resp)
	}
	var written sandboxapi.SandboxAgentMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, sandboxmeta.MaxFileSize*2)).Decode(&written); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadGateway, fmt.Sprintf("the sandbox answered a meta change with an unreadable response: %v", err))
	}
	meta := sandboxmeta.Meta{Description: written.Meta.Description.Or(""), Tags: map[string]string(written.Meta.Tags)}
	if err := meta.Validate(); err != nil {
		return nil, apperrors.NewStatusError(http.StatusBadGateway, fmt.Sprintf("the sandbox reported invalid meta: %v", err))
	}
	if err := s.store.UpdateSandboxMeta(ctx, sandboxModel.ProjectID, sandboxModel.ID, meta, written.ObservedAt); err != nil {
		return nil, err
	}
	return s.GetSandbox(ctx, projectID, sandboxID)
}

// sandboxMetaError passes on why the sandbox refused a meta change. What it
// refuses as the caller's doing — an invalid change, or a meta file that is
// not valid — keeps its status; anything else is the sandbox failing, which to
// this caller is a bad gateway.
func sandboxMetaError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	message := strings.TrimSpace(string(data))
	var decoded struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &decoded) == nil && strings.TrimSpace(decoded.Error) != "" {
		message = strings.TrimSpace(decoded.Error)
	}
	if message == "" {
		message = resp.Status
	}
	switch resp.StatusCode {
	case http.StatusBadRequest, http.StatusConflict:
		return apperrors.NewStatusError(resp.StatusCode, message)
	default:
		return apperrors.NewStatusError(http.StatusBadGateway, "the sandbox could not change its meta: "+message)
	}
}

// matchingTags keeps the sandboxes whose recorded tags satisfy every selector.
// It filters the listing here rather than in the query: tags are a JSON column,
// and a listing is one project's sandboxes, which are few enough to read.
func matchingTags(sandboxes []model.Sandbox, selectors []sandboxmeta.Selector) []model.Sandbox {
	if len(selectors) == 0 {
		return sandboxes
	}
	out := sandboxes[:0]
	for _, sandbox := range sandboxes {
		if sandboxmeta.MatchesAll(sandbox.Tags, selectors) {
			out = append(out, sandbox)
		}
	}
	return out
}
