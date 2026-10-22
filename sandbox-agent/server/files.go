package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	sandboxapi "github.com/discobox-ai/discobox/api/sandboxgen"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/files"
)

// The files API (ADR 26-10-22-499). Every operation acts as the discobox's
// default user, resolved as an exec that names nobody is, and its paths are
// relative to that user's home.

func (h *handler) StatSandboxFile(ctx context.Context, params sandboxapi.StatSandboxFileParams) (*sandboxapi.SandboxFileStat, error) {
	result, _, err := h.doFiles(ctx, files.Request{Op: files.OpStat}, params.Path, "", nil)
	if err != nil {
		return nil, err
	}
	return fileStatToAPI(result.Stat), nil
}

func (h *handler) ListSandboxFiles(ctx context.Context, params sandboxapi.ListSandboxFilesParams) (*sandboxapi.SandboxFileList, error) {
	req := files.Request{Op: files.OpList}
	result, _, err := h.doFiles(ctx, req, params.Path, "", nil)
	if err != nil {
		return nil, err
	}
	out := &sandboxapi.SandboxFileList{Path: result.Stat.Path, Entries: make([]sandboxapi.SandboxFileStat, 0, len(result.Entries))}
	for i := range result.Entries {
		out.Entries = append(out.Entries, *fileStatToAPI(&result.Entries[i]))
	}
	return out, nil
}

func (h *handler) ReadSandboxFile(ctx context.Context, params sandboxapi.ReadSandboxFileParams) (sandboxapi.ReadSandboxFileOK, error) {
	req := files.Request{Op: files.OpRead, Offset: params.Offset.Or(0), Length: -1}
	if length, ok := params.Length.Get(); ok {
		req.Length = length
	}
	_, content, err := h.doFiles(ctx, req, params.Path, "", nil)
	if err != nil {
		return sandboxapi.ReadSandboxFileOK{}, err
	}
	return sandboxapi.ReadSandboxFileOK{Data: content}, nil
}

func (h *handler) WriteSandboxFile(ctx context.Context, body sandboxapi.WriteSandboxFileReq, params sandboxapi.WriteSandboxFileParams) (*sandboxapi.SandboxFileStat, error) {
	req := files.Request{
		Op:            files.OpWrite,
		FailIfExists:  params.IfExists.Or(sandboxapi.WriteSandboxFileIfExistsReplace) == sandboxapi.WriteSandboxFileIfExistsFail,
		FailIfMissing: params.IfMissing.Or(sandboxapi.WriteSandboxFileIfMissingCreate) == sandboxapi.WriteSandboxFileIfMissingFail,
		IfVersion:     params.IfVersion.Or(""),
		Mode:          fileMode(params.Mode),
		Parents:       params.Parents.Or(false),
	}
	content := body.Data
	if content == nil {
		content = strings.NewReader("")
	}
	result, _, err := h.doFiles(ctx, req, params.Path, "", content)
	if err != nil {
		return nil, err
	}
	return fileStatToAPI(result.Stat), nil
}

func (h *handler) MakeSandboxDirectory(ctx context.Context, body *sandboxapi.SandboxFileMkdirRequest, _ sandboxapi.MakeSandboxDirectoryParams) (*sandboxapi.SandboxFileStat, error) {
	req := files.Request{Op: files.OpMkdir, Parents: body.Parents.Or(false), Mode: fileMode(body.Mode)}
	result, _, err := h.doFiles(ctx, req, body.Path, "", nil)
	if err != nil {
		return nil, err
	}
	return fileStatToAPI(result.Stat), nil
}

func (h *handler) RemoveSandboxFile(ctx context.Context, body *sandboxapi.SandboxFileRemoveRequest, _ sandboxapi.RemoveSandboxFileParams) error {
	_, _, err := h.doFiles(ctx, files.Request{Op: files.OpRemove, Recursive: body.Recursive.Or(false)}, body.Path, "", nil)
	return err
}

func (h *handler) MoveSandboxFile(ctx context.Context, body *sandboxapi.SandboxFileTransferRequest, _ sandboxapi.MoveSandboxFileParams) (*sandboxapi.SandboxFileStat, error) {
	result, _, err := h.doFiles(ctx, files.Request{Op: files.OpMove, Replace: body.Replace.Or(false)}, body.From, body.To, nil)
	if err != nil {
		return nil, err
	}
	return fileStatToAPI(result.Stat), nil
}

func (h *handler) CopySandboxFile(ctx context.Context, body *sandboxapi.SandboxFileTransferRequest, _ sandboxapi.CopySandboxFileParams) (*sandboxapi.SandboxFileStat, error) {
	result, _, err := h.doFiles(ctx, files.Request{Op: files.OpCopy, Replace: body.Replace.Or(false)}, body.From, body.To, nil)
	if err != nil {
		return nil, err
	}
	return fileStatToAPI(result.Stat), nil
}

// doFiles resolves the default user and the paths against that user's home,
// and performs req as that user.
func (h *handler) doFiles(ctx context.Context, req files.Request, path, to string, body io.Reader) (files.Result, io.ReadCloser, error) {
	if h.files == nil {
		return files.Result{}, nil, statusError{status: http.StatusServiceUnavailable, message: "the files API is not available in this sandbox"}
	}
	user, home, err := h.filesUser()
	if err != nil {
		return files.Result{}, nil, statusError{status: http.StatusInternalServerError, message: "resolve the sandbox's default user: " + err.Error()}
	}
	if req.Path, err = files.Resolve(path, home); err != nil {
		return files.Result{}, nil, filesStatusError(err)
	}
	if req.Op == files.OpMove || req.Op == files.OpCopy {
		if req.To, err = files.Resolve(to, home); err != nil {
			return files.Result{}, nil, filesStatusError(err)
		}
	}
	// A write is use, and holds the idle stop off while it runs; its end is
	// activity. A stat, a listing or a read is not: reading is never activity
	// (sandbox-agent/REVIEW.md), and an editor left open on a discobox would
	// keep it up forever.
	release := func() {}
	switch req.Op {
	case files.OpStat, files.OpList, files.OpRead:
	default:
		release = h.autostop.Hold("files " + string(req.Op) + " " + req.Path)
	}
	defer release()
	result, content, err := h.files.Do(ctx, user, req, body)
	if err != nil {
		return files.Result{}, nil, filesStatusError(err)
	}
	return result, content, nil
}

// filesUser is who the files API acts as: the default user an exec that names
// nobody runs as (nil when that is the agent's own identity), and that user's
// home.
func (h *handler) filesUser() (*execs.User, string, error) {
	user, err := h.execs.ResolveUser(execs.CreateRequest{})
	if err != nil {
		return nil, "", err
	}
	if user != nil {
		return user, strings.TrimSpace(user.HomeDirectory), nil
	}
	home, _ := os.UserHomeDir()
	return nil, home, nil
}

func fileMode(mode sandboxapi.OptInt64) *uint32 {
	value, ok := mode.Get()
	if !ok {
		return nil
	}
	out := uint32(value)
	return &out
}

func filesStatusError(err error) error {
	var fileErr *files.Error
	if !errors.As(err, &fileErr) {
		return statusError{status: http.StatusInternalServerError, message: err.Error()}
	}
	status := http.StatusInternalServerError
	switch fileErr.Kind {
	case files.KindInvalid:
		status = http.StatusBadRequest
	case files.KindNotFound:
		status = http.StatusNotFound
	case files.KindExists, files.KindConflict:
		status = http.StatusConflict
	case files.KindPermission:
		status = http.StatusForbidden
	case files.KindPrecondition:
		status = http.StatusPreconditionFailed
	}
	return statusError{status: status, message: fileErr.Message}
}

func fileStatToAPI(in *files.Stat) *sandboxapi.SandboxFileStat {
	if in == nil {
		return &sandboxapi.SandboxFileStat{}
	}
	out := &sandboxapi.SandboxFileStat{
		Path:       in.Path,
		Name:       in.Name,
		Type:       sandboxapi.SandboxFileType(in.Type),
		Size:       in.Size,
		Mode:       int64(in.Mode),
		ModifiedAt: in.ModifiedAt,
		Version:    in.Version,
	}
	if in.LinkTarget != "" {
		out.LinkTarget = sandboxapi.NewOptString(in.LinkTarget)
	}
	return out
}
