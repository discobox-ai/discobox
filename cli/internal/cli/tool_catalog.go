package cli

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/tools"
)

// A tool is a declaration in one of four layers, the last winning on a shared
// id (ADR 0125): what this CLI embeds, what the discobox's image and primary
// source declare, and what the user declares in their own config directory.
// The middle two are the sandbox's to list; this file merges them with the two
// that live here.

//go:embed builtin-tools
var builtinToolsFS embed.FS

// builtinTools is the CLI's own layer: the editors on this machine.
func builtinTools() []tools.Definition {
	sub, err := fs.Sub(builtinToolsFS, "builtin-tools")
	if err != nil {
		panic(err)
	}
	defs, err := tools.DiscoverFS(sub, "", tools.LayerBuiltin)
	if err != nil {
		panic(err)
	}
	return defs
}

// userTools is the user's layer: the directory the tools' config copies already
// live under, so a tool's declaration and the files it carries sit together. A
// config directory that cannot be resolved is a machine with no user layer, not
// a failure to list anything else.
func userTools() ([]tools.Definition, error) {
	dir, err := toolConfigDir()
	if err == nil {
		return tools.Discover(dir, tools.LayerUser)
	}
	return nil, nil
}

// reservedToolIDs are the names `discobox tools` already answers to itself.
var reservedToolIDs = map[string]bool{"git": true, "ssh": true, "ls": true, "list": true, "help": true}

// mergeTools is the catalog: the layers in order, with the CLI's own names
// refused rather than shadowed.
func mergeTools(layers ...[]tools.Definition) []tools.Definition {
	merged := tools.Merge(layers...)
	for i, def := range merged {
		if reservedToolIDs[def.ID] && def.Problem == "" {
			merged[i].Problem = fmt.Sprintf("%q is a command of discobox tools already; declare the tool under another name", def.ID)
		}
	}
	return merged
}

// localTools is the catalog before any discobox is asked: the builtin and user
// layers, which are all the tools that run on this machine. They become
// `discobox tools` subcommands, so help and completion know them.
func localTools() ([]tools.Definition, error) {
	user, err := userTools()
	if err != nil {
		return nil, err
	}
	return mergeTools(builtinTools(), user), nil
}

// toolCatalog is every tool one discobox can be worked on with.
func (a *App) toolCatalog(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) ([]tools.Definition, error) {
	boxed, err := sandboxTools(ctx, client, projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	var image, source []tools.Definition
	for _, def := range boxed {
		if def.Layer == tools.LayerImage {
			image = append(image, def)
		} else {
			source = append(source, def)
		}
	}
	user, err := userTools()
	if err != nil {
		return nil, err
	}
	return mergeTools(builtinTools(), image, source, user), nil
}

// findTool is the tool a person named in a catalog — its id, or its name when
// exactly one tool wears it — ready to run.
func findTool(catalog []tools.Definition, id string) (tools.Definition, error) {
	def, ok := tools.Lookup(catalog, id)
	if !ok {
		return tools.Definition{}, fmt.Errorf("no tool %q; `discobox tools ls` lists them", id)
	}
	if def.Problem != "" {
		return tools.Definition{}, fmt.Errorf("tool %s cannot run: %s", id, def.Problem)
	}
	return def, nil
}

// sandboxTools is what the discobox's image and primary source declare.
//
// A sandbox that predates the route — an older server, an older sandbox agent —
// declares no tools, rather than failing every tool including the ones that
// never needed it to answer (ADR 0125 §7). So does one that says tools are
// not available in it.
func sandboxTools(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) ([]tools.Definition, error) {
	res, err := client.ListSandboxTools(ctx, apiclientgen.ListSandboxToolsParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		// A route the server has never heard of answers with net/http's own
		// plain-text 404. Only that one: a pool agent that says "sandbox not
		// found" in plain text is answering the route, and hiding it would
		// list this CLI's tools for a discobox that could run none of them.
		var plain *plainStatusError
		if errors.As(err, &plain) && plain.code == http.StatusNotFound && plain.body == "404 page not found" {
			return nil, nil
		}
		return nil, err
	}
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok &&
		(problem.StatusCode == http.StatusNotFound || problem.StatusCode == http.StatusNotImplemented) {
		return nil, nil
	}
	body, err := expectResponse[apimodel.SandboxToolsResponse](res)
	if err != nil {
		return nil, err
	}
	out := make([]tools.Definition, 0, len(body.Tools))
	for _, tool := range body.Tools {
		out = append(out, definitionFromAPI(tool))
	}
	return out, nil
}

func definitionFromAPI(in apimodel.SandboxTool) tools.Definition {
	def := tools.Definition{
		ID:          in.ID,
		Name:        in.Name,
		Description: in.Description.Value,
		Key:         in.Key.Value,
		Runs:        tools.Runs(in.Runs),
		Layer:       tools.Layer(in.Layer),
		FileName:    in.FileName,
		Path:        in.Path.Value,
		Script:      in.Script,
		Program:     in.Program,
		Args:        in.Args,
		Env:         in.Env,
		Problem:     in.Problem.Value,
	}
	for _, file := range in.Files {
		def.Files = append(def.Files, tools.File{Name: file.Name, Home: file.Home, Default: file.Default.Value})
	}
	// The sandbox's word is checked again here, where it matters: anything with
	// root in the box can answer this route, so an id that is a path, a file
	// name that climbs out of the config directory, a .yaml with no program, or
	// a program for this machine is refused whatever the agent said about it.
	// Only the two layers a sandbox has are taken from it.
	if def.Layer != tools.LayerImage && def.Layer != tools.LayerSource {
		def.Problem = fmt.Sprintf("layer %q is not one a discobox declares", def.Layer)
	}
	if def.Problem == "" {
		def.Problem = def.Check()
	}
	return def
}
