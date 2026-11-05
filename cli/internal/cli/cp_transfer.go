package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/copyarchive"
)

// copyOperands owns operand semantics; every remote byte stream uses the
// ordinary authenticated API transport without a generated client's buffering.
func (a *App) copyOperands(ctx context.Context, client *api.Client, project string, operands []cpOperand, options copyOptions) error {
	destination := operands[len(operands)-1]
	dstInfo, dstErr := copyStat(ctx, client, project, destination, false)
	if dstErr != nil && !errors.Is(dstErr, fs.ErrNotExist) {
		return dstErr
	}
	directory := dstErr == nil && dstInfo.Type == api.SandboxFileTypeDirectory
	if len(operands) > 2 && !directory {
		return fmt.Errorf("multiple sources require an existing destination directory")
	}
	if trailingSlash(destination.path) && !directory {
		return fmt.Errorf("a trailing slash requires an existing destination directory")
	}
	for _, source := range operands[:len(operands)-1] {
		stat, err := copyStat(ctx, client, project, source, true)
		if err != nil {
			return err
		}
		isDir := stat.Type == api.SandboxFileTypeDirectory
		if trailingSlash(source.path) && !isDir {
			return fmt.Errorf("source trailing slash requires a directory")
		}
		if stat.Type != api.SandboxFileTypeFile && !isDir {
			return fmt.Errorf("special files cannot be copied: %s", terminalSafe(source.path))
		}
		if isDir && !options.recursive {
			return fmt.Errorf("directory %s requires -r", terminalSafe(source.path))
		}
		if options.preserve && !stat.AccessedAt.Set {
			return fmt.Errorf("source does not report access times; upgrade the discobox for -p")
		}
		dest := destination
		if directory {
			if err := copyarchive.Name(stat.Name); err != nil {
				return err
			}
			if strings.Contains(stat.Name, "/") {
				return fmt.Errorf("source basename is not one component")
			}
			if dest.remote {
				dest.path = path.Join(dest.path, stat.Name)
			} else {
				dest.path = filepath.Join(dest.path, stat.Name)
			}
		}
		if source.remote == dest.remote && source.reference == dest.reference {
			// Local real paths and remote canonical stat paths catch aliases and
			// source links, not merely identical operand strings.
			target, targetErr := copyStat(ctx, client, project, dest, true)
			if targetErr == nil && (stat.Path == target.Path || (stat.FileIdentity.Or("") != "" && stat.FileIdentity.Or("") == target.FileIdentity.Or(""))) {
				return fmt.Errorf("cannot copy a path onto itself")
			}
			if isDir {
				sourcePath := stat.ResolvedPath.Or(stat.Path)
				windowsPath := len(sourcePath) > 2 && sourcePath[1] == ':'
				destPath, err := canonicalCopyDestination(ctx, client, project, dest, windowsPath)
				if err != nil {
					return err
				}
				if windowsPath {
					sourcePath = strings.ReplaceAll(sourcePath, "\\", "/")
					destPath = strings.ReplaceAll(destPath, "\\", "/")
				}
				if destPath == sourcePath || strings.HasPrefix(destPath, strings.TrimRight(sourcePath, "/")+"/") {
					return fmt.Errorf("cannot copy a directory into itself")
				}
			}
		}
		existing, e := copyStat(ctx, client, project, dest, false)
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
		if e == nil && (existing.Type == api.SandboxFileTypeSymlink || existing.Type == api.SandboxFileTypeOther) {
			return fmt.Errorf("destination is a symlink or special file")
		}
		if e == nil && (existing.Type == api.SandboxFileTypeDirectory) != isDir {
			return fmt.Errorf("source and destination types conflict")
		}
		if isDir {
			err = a.copyDirectory(ctx, project, source, dest, options.preserve)
		} else {
			err = a.copyFile(ctx, client, project, source, dest, stat, options.preserve)
		}
		if err != nil {
			return fmt.Errorf("copy %s: %w", terminalSafe(source.path), err)
		}
	}
	return nil
}

func trailingSlash(p string) bool {
	return strings.HasSuffix(p, "/") || (filepath.Separator == '\\' && strings.HasSuffix(p, "\\"))
}

func copyStat(ctx context.Context, client *api.Client, project string, operand cpOperand, follow bool) (*api.SandboxFileStat, error) {
	if !operand.remote {
		name, err := filepath.Abs(operand.path)
		if err != nil {
			return nil, err
		}
		originalName := filepath.Base(name)
		if follow {
			name, err = filepath.EvalSymlinks(name)
			if err != nil {
				return nil, err
			}
		}
		info, err := os.Lstat(name)
		if err != nil {
			return nil, err
		}
		typ := api.SandboxFileTypeOther
		if info.Mode().IsRegular() {
			typ = api.SandboxFileTypeFile
		} else if info.IsDir() {
			typ = api.SandboxFileTypeDirectory
		} else if info.Mode()&fs.ModeSymlink != 0 {
			typ = api.SandboxFileTypeSymlink
		}
		meta := copyarchive.Info(info)
		identity, err := copyarchive.FileIdentity(name, info)
		if err != nil {
			return nil, err
		}
		return &api.SandboxFileStat{Path: name, ResolvedPath: api.NewOptString(name), FileIdentity: api.NewOptString(identity), Name: originalName, Type: typ, Mode: int64(meta.Mode), Size: info.Size(), ModifiedAt: meta.ModifiedAt, AccessedAt: api.NewOptDateTime(meta.AccessedAt)}, nil
	}
	originalName := ""
	for range 40 {
		res, err := client.StatSandboxFile(ctx, api.StatSandboxFileParams{ProjectId: project, SandboxId: operand.reference, Path: operand.path})
		if err != nil {
			return nil, sandboxFileCallError(err)
		}
		if problem, ok := res.(*api.ErrorResponseStatusCode); ok && problem.StatusCode == http.StatusNotFound {
			return nil, fs.ErrNotExist
		}
		stat, err := sandboxFileResponse(res)
		if err != nil {
			return nil, err
		}
		if originalName == "" {
			originalName = stat.Name
		}
		if !follow || stat.Type != api.SandboxFileTypeSymlink {
			if follow {
				stat.Name = originalName
			}
			return stat, nil
		}
		target := stat.LinkTarget.Or("")
		if !path.IsAbs(target) {
			target = path.Join(path.Dir(stat.Path), target)
		}
		operand.path = target
	}
	return nil, fmt.Errorf("source symlink chain exceeds 40 links")
}

// copyRequest returns a live body. The caller closes it even on cancellation;
// uploads never install GetBody, so consumed streams cannot be retried.
func (a *App) copyRequest(ctx context.Context, project string, operand cpOperand, suffix, method string, body io.Reader, options url.Values) (*http.Response, error) {
	base, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	q := options
	if q == nil {
		q = url.Values{}
	}
	q.Set("path", operand.path)
	endpoint := strings.TrimSuffix(base, "/") + "/api/projects/" + url.PathEscape(project) + "/sandboxes/" + url.PathEscape(operand.reference) + "/files/" + suffix + "?" + q.Encode()
	streamCtx, cancel := context.WithCancel(ctx)
	lifetime := &copyLifetime{cancel: cancel}
	lifetime.timer = time.AfterFunc(2*time.Minute, cancel)
	if body != nil {
		body = &copyProgressReader{reader: body, lifetime: lifetime}
	}
	req, err := http.NewRequestWithContext(streamCtx, method, endpoint, body)
	if err != nil {
		lifetime.close()
		return nil, err
	}
	req.GetBody = nil
	if suffix == "archive" {
		req.Header.Set("Content-Type", "application/x-tar")
	} else {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := client.Do(req)
	if err != nil {
		lifetime.close()
		return nil, sandboxFileCallError(err)
	}
	resp.Body = &copyProgressBody{ReadCloser: resp.Body, lifetime: lifetime}
	lifetime.touch()
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNotImplemented {
			return nil, fmt.Errorf("copy endpoint unavailable; upgrade the discobox, pool and server: %s", terminalSafe(string(data)))
		}
		return nil, fmt.Errorf("files API: %s: %s", resp.Status, terminalSafe(string(data)))
	}
	return resp, nil
}

func (a *App) copyDirectory(ctx context.Context, project string, source, dest cpOperand, preserve bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var body io.ReadCloser
	var producer <-chan error
	if source.remote {
		//nolint:bodyclose // The body is assigned below and closed on every exit.
		resp, err := a.copyRequest(ctx, project, source, "archive", http.MethodGet, nil, nil)
		if err != nil {
			return err
		}
		body = resp.Body
	} else {
		reader, writer := io.Pipe()
		body = reader
		done := make(chan error, 1)
		producer = done
		go func() { err := copyarchive.Export(ctx, writer, source.path); writer.CloseWithError(err); done <- err }()
	}
	defer body.Close()
	var err error
	if dest.remote {
		var resp *http.Response
		resp, err = a.copyRequest(ctx, project, dest, "archive", http.MethodPut, body, url.Values{"preserve": {fmt.Sprint(preserve)}})
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	} else {
		err = copyarchive.Import(ctx, body, dest.path, preserve)
	}
	cancel()
	body.Close()
	if producer != nil {
		if e := <-producer; err == nil {
			err = e
		}
	}
	return err
}

func (a *App) copyFile(ctx context.Context, client *api.Client, project string, source, dest cpOperand, stat *api.SandboxFileStat, preserve bool) error {
	var body io.ReadCloser
	if source.remote {
		//nolint:bodyclose // The body is assigned below and closed on every exit.
		resp, err := a.copyRequest(ctx, project, source, "content", http.MethodGet, nil, nil)
		if err != nil {
			return err
		}
		body = resp.Body
	} else {
		file, err := copyarchive.OpenSourceFile(stat.Path)
		if err != nil {
			return err
		}
		body = file
	}
	defer body.Close()
	checked := &copyFileReader{reader: body, remaining: stat.Size}
	meta := copyarchive.Metadata{Mode: fs.FileMode(stat.Mode) & 0777, AccessedAt: stat.AccessedAt.Or(stat.ModifiedAt), ModifiedAt: stat.ModifiedAt}
	if !dest.remote {
		root, err := copyarchive.OpenDirectory(filepath.Dir(dest.path), false)
		if err != nil {
			return err
		}
		defer root.Close()
		return copyarchive.WriteFile(ctx, root, filepath.Base(dest.path), checked, meta, preserve)
	}
	resp, err := a.copyRequest(ctx, project, dest, "content", http.MethodPut, checked, url.Values{"copy": {"true"}, "mode": {fmt.Sprint(stat.Mode & 0777)}})
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	if preserve {
		result, err := client.SetSandboxFileMetadata(ctx, &api.SandboxFileMetadataRequest{Path: dest.path, Mode: api.NewOptInt64(int64(meta.Mode)), AccessedAt: stat.AccessedAt, ModifiedAt: api.NewOptDateTime(meta.ModifiedAt)}, api.SetSandboxFileMetadataParams{ProjectId: project, SandboxId: dest.reference})
		if err != nil {
			return sandboxFileCallError(err)
		}
		if problem, ok := result.(*api.ErrorResponseStatusCode); ok && (problem.StatusCode == http.StatusNotImplemented || problem.StatusCode == http.StatusNotFound) {
			return fmt.Errorf("metadata preservation is unavailable; upgrade the discobox, pool and server")
		}
		_, err = sandboxFileResponse(result)
		return err
	}
	return nil
}

// Progress bounds setup and stalled content, while permitting arbitrarily long
// progressing transfers. Closing a response releases its timer and context.
type copyLifetime struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel context.CancelFunc
	closed bool
}

func (l *copyLifetime) touch() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.timer.Reset(2 * time.Minute)
	}
}
func (l *copyLifetime) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.timer.Stop()
		l.cancel()
	}
}

type copyProgressReader struct {
	reader   io.Reader
	lifetime *copyLifetime
}

func (r *copyProgressReader) Read(p []byte) (int, error) {
	n, e := r.reader.Read(p)
	if n > 0 {
		r.lifetime.touch()
	}
	return n, e
}

type copyProgressBody struct {
	io.ReadCloser
	lifetime *copyLifetime
}

func (b *copyProgressBody) Read(p []byte) (int, error) {
	n, e := b.ReadCloser.Read(p)
	if n > 0 {
		b.lifetime.touch()
	}
	return n, e
}
func (b *copyProgressBody) Close() error { b.lifetime.close(); return b.ReadCloser.Close() }

// Resolve the nearest existing destination ancestor, retaining missing suffixes.
// This catches a tree copied into itself through an alias even when several
// destination directories do not exist yet.
func canonicalCopyDestination(ctx context.Context, client *api.Client, project string, dest cpOperand, windowsPath bool) (string, error) {
	probe := dest
	if windowsPath {
		probe.path = strings.ReplaceAll(probe.path, "\\", "/")
	}
	var suffix []string
	for range 256 {
		stat, err := copyStat(ctx, client, project, probe, true)
		if err == nil {
			resolved := stat.ResolvedPath.Or(stat.Path)
			if windowsPath {
				resolved = strings.ReplaceAll(resolved, "\\", "/")
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				if dest.remote {
					resolved = path.Join(resolved, suffix[i])
				} else {
					resolved = filepath.Join(resolved, suffix[i])
				}
			}
			return filepath.ToSlash(resolved), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent, base := path.Dir(probe.path), path.Base(probe.path)
		if !dest.remote {
			parent, base = filepath.Dir(probe.path), filepath.Base(probe.path)
		}
		if parent == probe.path {
			return "", err
		}
		suffix = append(suffix, base)
		probe.path = parent
	}
	return "", fmt.Errorf("destination nesting exceeds 256")
}

// A proxy can turn an interrupted upstream body into a clean HTTP EOF. Stat
// supplies the copy's expected extent: never publish a short file even when
// the last hop reports success. An unstable source that grows also fails.
type copyFileReader struct {
	reader    io.Reader
	remaining int64
	complete  bool
}

func (r *copyFileReader) Read(p []byte) (int, error) {
	if r.complete {
		return 0, io.EOF
	}
	if r.remaining < 0 {
		return 0, fmt.Errorf("negative source file size")
	}
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("source grew during copy")
		}
		if errors.Is(err, io.EOF) {
			r.complete = true
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	if errors.Is(err, io.EOF) {
		if r.remaining > 0 {
			return n, io.ErrUnexpectedEOF
		}
		r.complete = true
	}
	return n, err
}
