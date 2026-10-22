package ahplocal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/discobox-ai/discobox/pkg/ahphost"
	"github.com/discobox-ai/discobox/sandbox-agent/files"
)

// The discobox's files, for the in-box host, as the files API does them
// (ADR 26-10-22-499): the same operations, in this process, which runs as the
// discobox's default user — the helper the agent starts for the API is that
// user too. So a file reads, writes and refuses here as it does through the
// control plane.

func (b *Boxes) StatFile(_ context.Context, _, path string) (ahphost.FileInfo, error) {
	result, _, err := files.Do(files.Request{Op: files.OpStat, Path: path}, nil)
	if err != nil {
		return ahphost.FileInfo{}, fileError(err, false)
	}
	return fileInfo(*result.Stat), nil
}

func (b *Boxes) ListFiles(_ context.Context, _, path string) ([]ahphost.FileInfo, error) {
	result, _, err := files.Do(files.Request{Op: files.OpList, Path: path}, nil)
	if err != nil {
		return nil, fileError(err, false)
	}
	out := make([]ahphost.FileInfo, 0, len(result.Entries))
	for _, entry := range result.Entries {
		out = append(out, fileInfo(entry))
	}
	return out, nil
}

func (b *Boxes) ReadFile(_ context.Context, _, path string) ([]byte, error) {
	_, content, err := files.Do(files.Request{Op: files.OpRead, Path: path, Length: -1}, nil)
	if err != nil {
		return nil, fileError(err, false)
	}
	defer content.Close()
	return io.ReadAll(content)
}

func (b *Boxes) WriteFile(_ context.Context, _, path string, write ahphost.FileWrite) error {
	_, _, err := files.Do(files.Request{Op: files.OpWrite, Path: path, FailIfExists: write.CreateOnly, IfVersion: write.IfVersion}, bytes.NewReader(write.Data))
	return fileError(err, write.CreateOnly)
}

func (b *Boxes) MakeDirectory(_ context.Context, _, path string) error {
	_, _, err := files.Do(files.Request{Op: files.OpMkdir, Path: path, Parents: true}, nil)
	return fileError(err, false)
}

func (b *Boxes) RemoveFile(_ context.Context, _, path string, recursive bool) error {
	_, _, err := files.Do(files.Request{Op: files.OpRemove, Path: path, Recursive: recursive}, nil)
	return fileError(err, false)
}

func (b *Boxes) MoveFile(_ context.Context, _, from, to string, replace bool) error {
	_, _, err := files.Do(files.Request{Op: files.OpMove, Path: from, To: to, Replace: replace}, nil)
	return fileError(err, false)
}

func (b *Boxes) CopyFile(_ context.Context, _, from, to string, replace bool) error {
	_, _, err := files.Do(files.Request{Op: files.OpCopy, Path: from, To: to, Replace: replace}, nil)
	return fileError(err, false)
}

// fileError is a files operation's error as the AHP host takes it: wrapping
// the fs error its kind means. A precondition is a write's own condition
// refusing it — a file where it must create one, or one no longer at the
// version it names.
func fileError(err error, createOnly bool) error {
	if err == nil {
		return nil
	}
	var fileErr *files.Error
	if !errors.As(err, &fileErr) {
		return err
	}
	var kind error
	switch fileErr.Kind {
	case files.KindInvalid, files.KindConflict:
		kind = fs.ErrInvalid
	case files.KindNotFound:
		kind = fs.ErrNotExist
	case files.KindExists:
		kind = fs.ErrExist
	case files.KindPermission:
		kind = fs.ErrPermission
	case files.KindPrecondition:
		kind = ahphost.ErrChanged
		if createOnly {
			kind = fs.ErrExist
		}
	default:
		return err
	}
	return fmt.Errorf("%s: %w", fileErr.Message, kind)
}

func fileInfo(stat files.Stat) ahphost.FileInfo {
	return ahphost.FileInfo{
		Path:       stat.Path,
		Name:       stat.Name,
		Type:       ahphost.FileType(stat.Type),
		Size:       stat.Size,
		ModTime:    stat.ModifiedAt,
		Version:    stat.Version,
		LinkTarget: stat.LinkTarget,
	}
}
