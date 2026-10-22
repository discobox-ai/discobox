package ahphost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
)

// The resource* commands are a discobox's files, through Boxes, as its
// default user (ADR 26-10-22-499). A file URI names its discobox the way a
// session's folder does (workingDirectory): by its authority from the CLI,
// which serves every discobox, and not at all in a discobox, which serves
// itself. createResourceWatch is a Workspace's alone.

// resourceMethods are the resource* commands a host answers.
var resourceMethods = map[string]bool{
	"resourceRead": true, "resourceWrite": true, "resourceList": true,
	"resourceCopy": true, "resourceDelete": true, "resourceMove": true,
	"resourceResolve": true, "resourceMkdir": true, "resourceRequest": true,
}

// handleResource answers one resource* command.
func (h *Host) handleResource(ctx context.Context, method string, params json.RawMessage) (any, error) {
	boxes := h.cfg.Boxes
	switch method {
	case "resourceRead":
		var p ahptypes.ResourceReadParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.resourceRead(ctx, p)
	case "resourceWrite":
		var p ahptypes.ResourceWriteParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return ahptypes.ResourceWriteResult{}, h.resourceWrite(ctx, p)
	case "resourceList":
		var p ahptypes.ResourceListParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.resourceList(ctx, p.Uri)
	case "resourceCopy", "resourceMove":
		var p ahptypes.ResourceCopyParams // a move's params are the same
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		do := boxes.CopyFile
		if method == "resourceMove" {
			do = boxes.MoveFile
		}
		return struct{}{}, h.transfer(ctx, p.Source, p.Destination, p.FailIfExists, do)
	case "resourceDelete":
		var p ahptypes.ResourceDeleteParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		box, path, err := h.filePath(p.Uri)
		if err != nil {
			return nil, err
		}
		return ahptypes.ResourceDeleteResult{}, fsError(boxes.RemoveFile(ctx, box, path, p.Recursive != nil && *p.Recursive))
	case "resourceResolve":
		var p ahptypes.ResourceResolveParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.resourceResolve(ctx, p)
	case "resourceMkdir":
		var p ahptypes.ResourceMkdirParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		box, path, err := h.filePath(p.Uri)
		if err != nil {
			return nil, err
		}
		return ahptypes.ResourceMkdirResult{}, fsError(boxes.MakeDirectory(ctx, box, path))
	default:
		// resourceRequest. Access is decided on each operation, as the
		// discobox's user: there is nothing to grant ahead of it.
		var p ahptypes.ResourceRequestParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if _, _, err := h.filePath(p.Uri); err != nil {
			return nil, err
		}
		return ahptypes.ResourceRequestResult{}, nil
	}
}

// filePath is the discobox and the path a file URI names. A discobox that is
// not running is not started for it: looking never wakes one
// (ADR 26-10-17-354 §5), and a listing is looking.
func (h *Host) filePath(uri string) (string, string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", errorf(ahptypes.ErrorCodeInvalidParams, "%v", err)
	}
	if u.Scheme != "file" || !path.IsAbs(u.Path) {
		return "", "", errorf(ahptypes.ErrorCodeNotFound, "%s is not a file in a discobox", uri)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.boxFromCwd(uri)
	if s == nil {
		return "", "", errorf(ahptypes.ErrorCodeNotFound, "%s names no discobox", uri)
	}
	if !s.running() {
		return "", "", errorf(ahptypes.ErrorCodeInvalidRequest, "discobox %s is %s: start it to reach its files", s.box.Name, s.box.State)
	}
	p, ok := fromURIPath(sandboxpath.For(s.box.Platform), u.Path)
	if !ok {
		return "", "", errorf(ahptypes.ErrorCodeNotFound, "%s is not an absolute path in discobox %s", uri, s.box.Name)
	}
	return s.box.ID, p, nil
}

// pathsOf is how paths are spelled in a discobox: by its own platform's
// rules (sandboxpath), never guessed from a path's shape.
func (h *Host) pathsOf(box string) sandboxpath.Paths {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[h.boxSessions[box]]; s != nil {
		return sandboxpath.For(s.box.Platform)
	}
	return sandboxpath.For(platform.Platform{})
}

// A file URI's path is slash-separated and rooted: a Windows discobox's
// `C:\work` is `/C:/work` there, as every file URI spells a drive, and its
// `\\server\share` is `//server/share`. Which a path is comes from the
// discobox's platform, since on Linux `/C:/work` is a directory named `C:`.

// toURIPath is a path in a discobox as a file URI's path.
func toURIPath(paths sandboxpath.Paths, p string) string {
	cleaned := paths.Clean(p)
	if paths.OS() != "windows" {
		return cleaned
	}
	cleaned = strings.ReplaceAll(cleaned, `\`, "/")
	if len(cleaned) == 2 && cleaned[1] == ':' {
		cleaned += "/"
	}
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

// fromURIPath is a file URI's path as the discobox spells it, and whether it
// is an absolute path there.
func fromURIPath(paths sandboxpath.Paths, p string) (string, bool) {
	if paths.OS() == "windows" && !strings.HasPrefix(p, "//") {
		p = strings.TrimPrefix(p, "/")
		if len(p) == 2 && p[1] == ':' {
			p += "/"
		}
	}
	cleaned := paths.Clean(p)
	return cleaned, paths.IsAbs(cleaned)
}

// fileURI is the URI a path in a discobox has, as filePath reads it.
func (h *Host) fileURI(box, p string) string {
	u := url.URL{Scheme: "file", Path: toURIPath(h.pathsOf(box), p)}
	if h.cfg.Workspace == nil {
		u.Host = box
	}
	return u.String()
}

// fsError is err as the protocol says it: a missing file is NotFound,
// which is how a client asks whether one exists.
func fsError(err error) error {
	var rerr *rpcError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rerr):
		return err
	case errors.Is(err, fs.ErrNotExist):
		return errorf(ahptypes.ErrorCodeNotFound, "%v", err)
	case errors.Is(err, fs.ErrPermission):
		return errorf(ahptypes.ErrorCodePermissionDenied, "%v", err)
	case errors.Is(err, fs.ErrExist):
		return errorf(ahptypes.ErrorCodeAlreadyExists, "%v", err)
	case errors.Is(err, fs.ErrInvalid):
		return errorf(ahptypes.ErrorCodeInvalidParams, "%v", err)
	case errors.Is(err, ErrChanged):
		return errorf(ahptypes.ErrorCodeInvalidRequest, "%v", err)
	}
	return errorf(ahptypes.ErrorCodeInternalError, "%v", err)
}

// resourceRead reads a file, or a file as a changeset's base revision has it
// (changesets.go, baseContentURI).
func (h *Host) resourceRead(ctx context.Context, p ahptypes.ResourceReadParams) (any, error) {
	var (
		data []byte
		err  error
	)
	if strings.HasPrefix(p.Uri, baseContentScheme+":") {
		data, err = h.readBase(ctx, p.Uri)
	} else {
		box, file, perr := h.filePath(p.Uri)
		if perr != nil {
			return nil, perr
		}
		data, err = h.cfg.Boxes.ReadFile(ctx, box, file)
	}
	if err != nil {
		return nil, fsError(err)
	}
	contentType := contentTypeOf(p.Uri, data)
	result := ahptypes.ResourceReadResult{Data: base64.StdEncoding.EncodeToString(data), Encoding: ahptypes.ContentEncodingBase64, ContentType: &contentType}
	if p.Encoding != nil && *p.Encoding == ahptypes.ContentEncodingUtf8 && isText(data) {
		result.Data, result.Encoding = string(data), ahptypes.ContentEncodingUtf8
	}
	return result, nil
}

// contentTypeOf is a file's type by its extension when that agrees with
// what it holds: text stays text whatever its extension says (go.mod is not
// an audio module).
func contentTypeOf(uri string, data []byte) string {
	byExt := ""
	if u, err := url.Parse(uri); err == nil {
		byExt = mime.TypeByExtension(path.Ext(u.Path))
	}
	if isText(data) {
		if strings.HasPrefix(byExt, "text/") {
			return byExt
		}
		return "text/plain; charset=utf-8"
	}
	if byExt != "" {
		return byExt
	}
	return http.DetectContentType(data)
}

func isText(data []byte) bool {
	return !bytes.ContainsRune(data, 0) && strings.ToValidUTF8(string(data), "�") == string(data)
}

// resourceWrite writes a file. A file is written whole, so a write that keeps
// some of what is there — appended, inserted, or truncated past its start —
// reads it first and is written only if nothing changed it in between.
func (h *Host) resourceWrite(ctx context.Context, p ahptypes.ResourceWriteParams) error {
	box, file, err := h.filePath(p.Uri)
	if err != nil {
		return err
	}
	data := []byte(p.Data)
	if p.Encoding == ahptypes.ContentEncodingBase64 {
		if data, err = base64.StdEncoding.DecodeString(p.Data); err != nil {
			return errorf(ahptypes.ErrorCodeInvalidParams, "data: %v", err)
		}
	}
	mode := ahptypes.ResourceWriteModeTruncate
	if p.Mode != nil {
		mode = *p.Mode
	}
	var position int64
	if p.Position != nil {
		position = *p.Position
	}
	if position < 0 {
		return errorf(ahptypes.ErrorCodeInvalidParams, "position %d is negative", position)
	}
	write := FileWrite{Data: data, CreateOnly: p.CreateOnly != nil && *p.CreateOnly}
	if p.IfMatch != nil {
		write.IfVersion = *p.IfMatch
	}
	if mode != ahptypes.ResourceWriteModeTruncate || position > 0 {
		if write.Data, write.IfVersion, err = h.splice(ctx, box, file, write, mode, position); err != nil {
			return fsError(err)
		}
	}
	return fsError(h.cfg.Boxes.WriteFile(ctx, box, file, write))
}

// splice places a write's data in the file as it is: from position on,
// truncating; at position; or position bytes back from the end. It answers
// the whole content and the version it was read at.
func (h *Host) splice(ctx context.Context, box, file string, write FileWrite, mode ahptypes.ResourceWriteMode, position int64) ([]byte, string, error) {
	info, err := h.cfg.Boxes.StatFile(ctx, box, file)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if position > 0 {
			return nil, "", fmt.Errorf("position %d is past the end of %s: %w", position, file, fs.ErrInvalid)
		}
		return write.Data, write.IfVersion, nil
	case err != nil:
		return nil, "", err
	case write.CreateOnly:
		return nil, "", fmt.Errorf("%s: %w", file, fs.ErrExist)
	case write.IfVersion != "" && write.IfVersion != info.Version:
		return nil, "", fmt.Errorf("%s: %w", file, ErrChanged)
	}
	old, err := h.cfg.Boxes.ReadFile(ctx, box, file)
	if err != nil {
		return nil, "", err
	}
	at := position
	if mode == ahptypes.ResourceWriteModeAppend {
		at = int64(len(old)) - position
	}
	if at < 0 || at > int64(len(old)) {
		return nil, "", fmt.Errorf("position %d is outside %s: %w", position, file, fs.ErrInvalid)
	}
	rest := old[at:]
	if mode == ahptypes.ResourceWriteModeTruncate {
		rest = nil
	}
	return append(append(append([]byte{}, old[:at]...), write.Data...), rest...), info.Version, nil
}

func (h *Host) resourceList(ctx context.Context, uri string) (any, error) {
	box, dir, err := h.filePath(uri)
	if err != nil {
		return nil, err
	}
	entries, err := h.cfg.Boxes.ListFiles(ctx, box, dir)
	if err != nil {
		return nil, fsError(err)
	}
	out := ahptypes.ResourceListResult{Entries: make([]ahptypes.DirectoryEntry, 0, len(entries))}
	for _, entry := range entries {
		// A link lists as what it points at, which is what a folder picker
		// opens and an explorer expands; the files API lists it as itself.
		if entry.Type == FileTypeSymlink {
			entry = h.followLink(ctx, box, dir, entry)
		}
		kind := ahptypes.ResourceTypeFile
		if entry.Type == FileTypeDirectory {
			kind = ahptypes.ResourceTypeDirectory
		}
		out.Entries = append(out.Entries, ahptypes.DirectoryEntry{Name: entry.Name, Type: string(kind)})
	}
	return out, nil
}

// followLink is what a symlink in dir points at, one link deep, keeping the
// link's name — or the link itself when what it points at cannot be stat'd.
func (h *Host) followLink(ctx context.Context, box, dir string, link FileInfo) FileInfo {
	if link.LinkTarget == "" {
		return link
	}
	paths := h.pathsOf(box)
	target := link.LinkTarget
	if !paths.IsAbs(target) {
		target = paths.Join(dir, target)
	}
	followed, err := h.cfg.Boxes.StatFile(ctx, box, paths.Clean(target))
	if err != nil {
		return link
	}
	followed.Name = link.Name
	return followed
}

// transfer copies or moves source to destination, within one discobox.
func (h *Host) transfer(ctx context.Context, source, destination string, failIfExists *bool, do func(context.Context, string, string, string, bool) error) error {
	box, from, err := h.filePath(source)
	if err != nil {
		return err
	}
	toBox, to, err := h.filePath(destination)
	if err != nil {
		return err
	}
	if toBox != box {
		return errorf(ahptypes.ErrorCodeInvalidParams, "%s and %s are in different discoboxes", source, destination)
	}
	return fsError(do(ctx, box, from, to, failIfExists == nil || !*failIfExists))
}

// resourceResolve stats a path, following a symlink — one link deep, which
// is what a folder picker and an editor opening a linked file need — unless
// told not to.
func (h *Host) resourceResolve(ctx context.Context, p ahptypes.ResourceResolveParams) (any, error) {
	box, file, err := h.filePath(p.Uri)
	if err != nil {
		return nil, err
	}
	info, err := h.cfg.Boxes.StatFile(ctx, box, file)
	if err != nil {
		return nil, fsError(err)
	}
	if (p.FollowSymlinks == nil || *p.FollowSymlinks) && info.Type == FileTypeSymlink {
		info = h.followLink(ctx, box, h.pathsOf(box).Dir(file), info)
	}
	out := ahptypes.ResourceResolveResult{Uri: p.Uri, Type: ahptypes.ResourceTypeFile, Mtime: ptr(timestamp(info.ModTime)), Etag: ptr(info.Version)}
	if paths := h.pathsOf(box); info.Path != "" && paths.Clean(info.Path) != file {
		out.Uri = h.fileURI(box, info.Path)
	}
	switch info.Type {
	case FileTypeDirectory:
		out.Type = ahptypes.ResourceTypeDirectory
	case FileTypeSymlink:
		out.Type = ahptypes.ResourceTypeSymlink
	default:
		out.Size = ptr(info.Size)
	}
	return out, nil
}
