package ahphost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// The resource* commands are the discobox's own filesystem, which a host has
// only when it runs in the discobox (Config.Workspace). The CLI's host has no
// discobox's filesystem to serve, and answers them as unserved.

// resourceMethods are the resource* commands a host with a Workspace answers.
var resourceMethods = map[string]bool{
	"resourceRead": true, "resourceWrite": true, "resourceList": true,
	"resourceCopy": true, "resourceDelete": true, "resourceMove": true,
	"resourceResolve": true, "resourceMkdir": true, "resourceRequest": true,
	"createResourceWatch": true,
}

// handleResource answers one resource* command.
func (h *Host) handleResource(ctx context.Context, c *conn, method string, params json.RawMessage) (any, error) {
	ws := h.cfg.Workspace
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
		return ahptypes.ResourceWriteResult{}, h.resourceWrite(p)
	case "resourceList":
		var p ahptypes.ResourceListParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.resourceList(p.Uri)
	case "resourceCopy":
		var p ahptypes.ResourceCopyParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return ahptypes.ResourceCopyResult{}, transfer(p.Source, p.Destination, p.FailIfExists, ws.Copy)
	case "resourceMove":
		var p ahptypes.ResourceMoveParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return ahptypes.ResourceMoveResult{}, transfer(p.Source, p.Destination, p.FailIfExists, ws.Move)
	case "resourceDelete":
		var p ahptypes.ResourceDeleteParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		path, err := localPath(p.Uri)
		if err != nil {
			return nil, err
		}
		return ahptypes.ResourceDeleteResult{}, fsError(ws.Remove(path, p.Recursive != nil && *p.Recursive))
	case "resourceResolve":
		var p ahptypes.ResourceResolveParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.resourceResolve(p)
	case "resourceMkdir":
		var p ahptypes.ResourceMkdirParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		path, err := localPath(p.Uri)
		if err != nil {
			return nil, err
		}
		return ahptypes.ResourceMkdirResult{}, fsError(ws.Mkdir(path))
	case "resourceRequest":
		// Access is the operating system's to decide, on each operation, for
		// the user the host runs as: there is nothing to grant ahead of it.
		var p ahptypes.ResourceRequestParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		if _, err := localPath(p.Uri); err != nil {
			return nil, err
		}
		return ahptypes.ResourceRequestResult{}, nil
	default:
		var p ahptypes.CreateResourceWatchParams
		if err := decode(params, &p); err != nil {
			return nil, err
		}
		return h.createResourceWatch(c, p)
	}
}

// localPath is the path a file URI names on this machine. A URI naming
// another machine, or any scheme but file, is not this host's to serve.
func localPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", errorf(ahptypes.ErrorCodeInvalidParams, "%v", err)
	}
	if u.Scheme != "file" || (u.Host != "" && u.Host != "localhost") {
		return "", errorf(ahptypes.ErrorCodeNotFound, "%s is not a file on this discobox", uri)
	}
	if !filepath.IsAbs(u.Path) {
		return "", errorf(ahptypes.ErrorCodeInvalidParams, "%s is not an absolute path", uri)
	}
	return filepath.Clean(u.Path), nil
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
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
		var path string
		if path, err = localPath(p.Uri); err != nil {
			return nil, err
		}
		data, err = h.cfg.Workspace.ReadFile(path)
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
		byExt = mime.TypeByExtension(filepath.Ext(u.Path))
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

func (h *Host) resourceWrite(p ahptypes.ResourceWriteParams) error {
	path, err := localPath(p.Uri)
	if err != nil {
		return err
	}
	write := FileWrite{Data: []byte(p.Data), Mode: ahptypes.ResourceWriteModeTruncate, CreateOnly: p.CreateOnly != nil && *p.CreateOnly}
	if p.Encoding == ahptypes.ContentEncodingBase64 {
		if write.Data, err = base64.StdEncoding.DecodeString(p.Data); err != nil {
			return errorf(ahptypes.ErrorCodeInvalidParams, "data: %v", err)
		}
	}
	if p.Mode != nil {
		write.Mode = *p.Mode
	}
	if p.Position != nil {
		write.Position = *p.Position
	}
	if write.Position < 0 {
		return errorf(ahptypes.ErrorCodeInvalidParams, "position %d is negative", write.Position)
	}
	if p.IfMatch != nil {
		write.IfMatch = *p.IfMatch
	}
	return fsError(h.cfg.Workspace.WriteFile(path, write))
}

func (h *Host) resourceList(uri string) (any, error) {
	path, err := localPath(uri)
	if err != nil {
		return nil, err
	}
	entries, err := h.cfg.Workspace.ReadDir(path)
	if err != nil {
		return nil, fsError(err)
	}
	out := ahptypes.ResourceListResult{Entries: make([]ahptypes.DirectoryEntry, 0, len(entries))}
	for _, entry := range entries {
		kind := ahptypes.ResourceTypeFile
		if entry.Dir {
			kind = ahptypes.ResourceTypeDirectory
		}
		out.Entries = append(out.Entries, ahptypes.DirectoryEntry{Name: entry.Name, Type: string(kind)})
	}
	return out, nil
}

// transfer copies or moves source to destination.
func transfer(source, destination string, failIfExists *bool, do func(from, to string, replace bool) error) error {
	from, err := localPath(source)
	if err != nil {
		return err
	}
	to, err := localPath(destination)
	if err != nil {
		return err
	}
	return fsError(do(from, to, failIfExists == nil || !*failIfExists))
}

func (h *Host) resourceResolve(p ahptypes.ResourceResolveParams) (any, error) {
	path, err := localPath(p.Uri)
	if err != nil {
		return nil, err
	}
	info, err := h.cfg.Workspace.Stat(path, p.FollowSymlinks == nil || *p.FollowSymlinks)
	if err != nil {
		return nil, fsError(err)
	}
	out := ahptypes.ResourceResolveResult{Uri: p.Uri, Type: info.Type, Mtime: ptr(timestamp(info.ModTime)), Etag: ptr(info.Etag)}
	if info.Path != path {
		out.Uri = fileURI(info.Path)
	}
	if info.Type == ahptypes.ResourceTypeFile {
		out.Size = ptr(info.Size)
	}
	return out, nil
}
