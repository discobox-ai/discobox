package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// The discobox's files API (ADR 26-10-22-499): what the CLI puts into a
// discobox it writes there as the discobox's default user, with the content
// as the request body, rather than through an exec of sh and cat.

// errSandboxFileExists is a write that must create, or a mkdir, refused
// because the path is already there.
var errSandboxFileExists = errors.New("already exists in the discobox")

// errNoFilesAPI is a discobox, or the server or pool on the way to it, older
// than the files API: the route answers with net/http's own plain-text 404.
// Upgrading the discobox onto a current image brings a sandbox agent that
// serves it.
var errNoFilesAPI = errors.New("this discobox has no files API: its sandbox agent, its pool or the server predates it; upgrade the discobox onto its harness config's current image")

// writeSandboxFile writes content to params.Path in the discobox.
func (a *App) writeSandboxFile(ctx context.Context, params apiclientgen.WriteSandboxFileParams, content io.Reader) (*apiclientgen.SandboxFileStat, error) {
	client, err := a.apiClient()
	if err != nil {
		return nil, err
	}
	res, err := client.WriteSandboxFile(ctx, apiclientgen.WriteSandboxFileReq{Data: content}, params)
	if err != nil {
		return nil, sandboxFileCallError(err)
	}
	// A write told to fail where a file is answers 412, as an If-None-Match
	// does: a 409 is also every refusal on the way to the discobox.
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok && problem.StatusCode == http.StatusPreconditionFailed && params.IfExists.Or("") == apiclientgen.WriteSandboxFileIfExistsFail {
		return nil, fmt.Errorf("%w: %s", errSandboxFileExists, terminalSafe(problem.Response.Error))
	}
	return sandboxFileResponse(res)
}

// makeSandboxDirectory creates a directory in the discobox.
func (a *App) makeSandboxDirectory(ctx context.Context, projectID, sandboxID string, body *apiclientgen.SandboxFileMkdirRequest) (*apiclientgen.SandboxFileStat, error) {
	client, err := a.apiClient()
	if err != nil {
		return nil, err
	}
	res, err := client.MakeSandboxDirectory(ctx, body, apiclientgen.MakeSandboxDirectoryParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, sandboxFileCallError(err)
	}
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok && problem.StatusCode == http.StatusConflict {
		// A 409 is also every refusal on the way to the discobox, so the
		// path's being there is asked rather than read off the status.
		if stat, statErr := client.StatSandboxFile(ctx, apiclientgen.StatSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID, Path: body.Path}); statErr == nil {
			if _, exists := stat.(*apiclientgen.SandboxFileStat); exists {
				return nil, fmt.Errorf("%w: %s", errSandboxFileExists, terminalSafe(problem.Response.Error))
			}
		}
	}
	return sandboxFileResponse(res)
}

// sandboxFileResponse is the stat a files API call answered, or its error.
// The error's text is the discobox's own, so it is escaped before anything
// can print it (cli/REVIEW.md).
func sandboxFileResponse(res any) (*apiclientgen.SandboxFileStat, error) {
	stat, err := expectResponse[apiclientgen.SandboxFileStat](res)
	if err != nil {
		return nil, errors.New(terminalSafe(err.Error()))
	}
	return stat, nil
}

// sandboxFileCallError is a files API call that got no answer the contract
// describes: the route missing on the way, said as such; another plain-text
// answer, escaped, since anything on the way may have written it; or the
// transport's own failure, as it is.
func sandboxFileCallError(err error) error {
	var plain *plainStatusError
	if !errors.As(err, &plain) {
		return err
	}
	if plain.code == http.StatusNotFound && plain.body == "404 page not found" {
		return errNoFilesAPI
	}
	return errors.New(terminalSafe(plain.Error()))
}
