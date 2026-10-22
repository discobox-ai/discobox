package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/ahphost"
)

// The discobox's files API (ADR 26-10-22-499): what the CLI puts into a
// discobox, and what the AHP host reads and writes there, it does as the
// discobox's default user, with the content as the request body, rather than
// through an exec of sh and cat.

// errSandboxFileExists is a write that must create, a mkdir, or a move or
// copy not to replace, refused because the path is already there.
var errSandboxFileExists = fmt.Errorf("already exists in the discobox: %w", fs.ErrExist)

// errSandboxFileChanged is a write that named a version the file is no
// longer at.
var errSandboxFileChanged = fmt.Errorf("changed in the discobox since it was read: %w", ahphost.ErrChanged)

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
	// A write's own condition refusing it answers 412, as an If-None-Match
	// does: a 409 is also every refusal on the way to the discobox.
	if problem, ok := res.(*apiclientgen.ErrorResponseStatusCode); ok && problem.StatusCode == http.StatusPreconditionFailed {
		switch {
		case params.IfExists.Or("") == apiclientgen.WriteSandboxFileIfExistsFail:
			return nil, fmt.Errorf("%w: %s", errSandboxFileExists, terminalSafe(problem.Response.Error))
		case params.IfVersion.IsSet():
			return nil, fmt.Errorf("%w: %s", errSandboxFileChanged, terminalSafe(problem.Response.Error))
		}
	}
	// 409 is a directory where the file would be.
	return sandboxFileAnswer[apiclientgen.SandboxFileStat](res, params.Path, fs.ErrInvalid, a.sandboxFileThere(ctx, client, params.ProjectId, params.SandboxId, params.Path))
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
	return sandboxFileAnswer[apiclientgen.SandboxFileStat](res, body.Path, errSandboxFileExists, a.sandboxFileThere(ctx, client, projectID, sandboxID, body.Path))
}

// sandboxFileAnswer is a files API answer: the value, or an error wrapping
// what its status means — fs.ErrInvalid, fs.ErrNotExist, fs.ErrPermission,
// or for a 409 whatever conflict the operation says it is. A 409 is also
// every refusal on the way to the discobox — its pool unreachable, the
// discobox archived — so it is the path's own only when there says the path
// is there to be in the way. The discobox's message is escaped, since
// anything may print it (cli/REVIEW.md).
func sandboxFileAnswer[T any](res any, path string, conflict error, there func() bool) (*T, error) {
	if out, ok := res.(*T); ok {
		return out, nil
	}
	problem, ok := res.(*apiclientgen.ErrorResponseStatusCode)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected answer %T", terminalSafe(path), res)
	}
	message := terminalSafe(problem.Response.Error)
	var kind error
	switch problem.StatusCode {
	case http.StatusBadRequest:
		kind = fs.ErrInvalid
	case http.StatusNotFound:
		kind = fs.ErrNotExist
	case http.StatusForbidden:
		kind = fs.ErrPermission
	case http.StatusConflict:
		if there == nil || !there() {
			return nil, errors.New(message)
		}
		kind = conflict
	default:
		return nil, fmt.Errorf("%d: %s", problem.StatusCode, message)
	}
	return nil, fmt.Errorf("%w: %s", kind, message)
}

// sandboxFileThere asks the discobox whether a path is there.
func (a *App) sandboxFileThere(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, path string) func() bool {
	return func() bool {
		res, err := client.StatSandboxFile(ctx, apiclientgen.StatSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID, Path: path})
		if err != nil {
			return false
		}
		_, there := res.(*apiclientgen.SandboxFileStat)
		return there
	}
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
