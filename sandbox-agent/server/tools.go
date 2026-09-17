package server

import (
	"context"
	"net/http"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	sandboxtools "github.com/discobox-ai/discobox/tools"
)

// toolDirs are where this sandbox's tools are declared: the image's, and the
// primary source's. Both empty is a sandbox whose working tree did not
// resolve, which answers "not available" rather than an empty listing.
type toolDirs struct {
	image  string
	source string
}

// ListSandboxTools lists what the image and the primary source declare. The
// sandbox runs none of it — which tools exist, and which of them win, is the
// client's to decide once it has merged in its own layers (ADR 0125 §7) — so
// this route translates declarations and nothing else.
func (h *handler) ListSandboxTools(context.Context, sandboxapi.ListSandboxToolsParams) (*sandboxapi.SandboxToolsResponse, error) {
	if h.tools == (toolDirs{}) {
		return nil, statusError{status: http.StatusNotImplemented, message: "sandbox tools are not available in this sandbox"}
	}
	response := sandboxapi.SandboxToolsResponse{Tools: []sandboxapi.SandboxTool{}}
	for _, dir := range []struct {
		path  string
		layer sandboxtools.Layer
	}{{h.tools.image, sandboxtools.LayerImage}, {h.tools.source, sandboxtools.LayerSource}} {
		defs, err := sandboxtools.Discover(dir.path, dir.layer)
		if err != nil {
			return nil, statusError{status: http.StatusInternalServerError, message: err.Error()}
		}
		for _, def := range defs {
			response.Tools = append(response.Tools, sandboxTool(def))
		}
	}
	return &response, nil
}

func sandboxTool(in sandboxtools.Definition) sandboxapi.SandboxTool {
	out := sandboxapi.SandboxTool{
		ID:       in.ID,
		Name:     in.Name,
		Runs:     sandboxapi.SandboxToolRuns(in.Runs),
		Layer:    sandboxapi.SandboxToolLayer(in.Layer),
		FileName: in.FileName,
		Script:   in.Script,
		Program:  in.Program,
		Args:     in.Args,
		Env:      in.Env,
	}
	if in.Description != "" {
		out.Description = sandboxapi.NewOptString(in.Description)
	}
	if in.Key != "" {
		out.Key = sandboxapi.NewOptString(in.Key)
	}
	if in.Path != "" {
		out.Path = sandboxapi.NewOptString(in.Path)
	}
	if in.Problem != "" {
		out.Problem = sandboxapi.NewOptString(in.Problem)
	}
	for _, file := range in.Files {
		entry := sandboxapi.SandboxToolFile{Name: file.Name, Home: file.Home}
		if file.Default != "" {
			entry.Default = sandboxapi.NewOptString(file.Default)
		}
		out.Files = append(out.Files, entry)
	}
	return out
}
