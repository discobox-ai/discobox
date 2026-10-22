package cli

import (
	"bytes"
	"context"
	"io"
	"io/fs"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/ahphost"
)

// A discobox's files for the AHP host, through the server's files API, as
// the discobox's default user (ADR 26-10-22-499 §4): what VS Code's
// resource* commands read and write for the sessions `discobox admin ahp`
// serves. Each call goes to the server the discobox is on, and its answer is
// read as every files API call is (sandbox_files.go).

// filesAt is the App, client and project of the server a discobox is on.
func (b ahpBoxes) filesAt(sandboxID string) (*App, *apiclientgen.Client, string, error) {
	d := b.ds.at(sandboxID)
	client, err := d.app.apiClient()
	return d.app, client, d.projectID, err
}

func (b ahpBoxes) StatFile(ctx context.Context, sandboxID, path string) (ahphost.FileInfo, error) {
	_, client, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return ahphost.FileInfo{}, err
	}
	res, err := client.StatSandboxFile(ctx, apiclientgen.StatSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID, Path: path})
	if err != nil {
		return ahphost.FileInfo{}, sandboxFileCallError(err)
	}
	stat, err := sandboxFileAnswer[apiclientgen.SandboxFileStat](res, path, fs.ErrInvalid, nil)
	if err != nil {
		return ahphost.FileInfo{}, err
	}
	return ahpFileInfo(*stat), nil
}

func (b ahpBoxes) ListFiles(ctx context.Context, sandboxID, path string) ([]ahphost.FileInfo, error) {
	app, client, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return nil, err
	}
	res, err := client.ListSandboxFiles(ctx, apiclientgen.ListSandboxFilesParams{ProjectId: projectID, SandboxId: sandboxID, Path: path})
	if err != nil {
		return nil, sandboxFileCallError(err)
	}
	// 409 is a path that is not a directory.
	list, err := sandboxFileAnswer[apiclientgen.SandboxFileList](res, path, fs.ErrInvalid, app.sandboxFileThere(ctx, client, projectID, sandboxID, path))
	if err != nil {
		return nil, err
	}
	out := make([]ahphost.FileInfo, 0, len(list.Entries))
	for _, entry := range list.Entries {
		out = append(out, ahpFileInfo(entry))
	}
	return out, nil
}

func (b ahpBoxes) ReadFile(ctx context.Context, sandboxID, path string) ([]byte, error) {
	app, client, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return nil, err
	}
	res, err := client.ReadSandboxFile(ctx, apiclientgen.ReadSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID, Path: path})
	if err != nil {
		return nil, sandboxFileCallError(err)
	}
	// 409 is a directory.
	content, err := sandboxFileAnswer[apiclientgen.ReadSandboxFileOK](res, path, fs.ErrInvalid, app.sandboxFileThere(ctx, client, projectID, sandboxID, path))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(content.Data)
}

func (b ahpBoxes) WriteFile(ctx context.Context, sandboxID, path string, write ahphost.FileWrite) error {
	app, _, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return err
	}
	params := apiclientgen.WriteSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID, Path: path}
	if write.CreateOnly {
		params.IfExists = apiclientgen.NewOptWriteSandboxFileIfExists(apiclientgen.WriteSandboxFileIfExistsFail)
	}
	if write.IfVersion != "" {
		params.IfVersion = apiclientgen.NewOptString(write.IfVersion)
	}
	_, err = app.writeSandboxFile(ctx, params, bytes.NewReader(write.Data))
	return err
}

func (b ahpBoxes) MakeDirectory(ctx context.Context, sandboxID, path string) error {
	app, _, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return err
	}
	_, err = app.makeSandboxDirectory(ctx, projectID, sandboxID, &apiclientgen.SandboxFileMkdirRequest{Path: path, Parents: apiclientgen.NewOptBool(true)})
	return err
}

func (b ahpBoxes) RemoveFile(ctx context.Context, sandboxID, path string, recursive bool) error {
	app, client, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return err
	}
	res, err := client.RemoveSandboxFile(ctx, &apiclientgen.SandboxFileRemoveRequest{Path: path, Recursive: apiclientgen.NewOptBool(recursive)}, apiclientgen.RemoveSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return sandboxFileCallError(err)
	}
	// 409 is a directory that is not empty.
	_, err = sandboxFileAnswer[apiclientgen.RemoveSandboxFileNoContent](res, path, fs.ErrInvalid, app.sandboxFileThere(ctx, client, projectID, sandboxID, path))
	return err
}

func (b ahpBoxes) MoveFile(ctx context.Context, sandboxID, from, to string, replace bool) error {
	return b.transferFile(ctx, sandboxID, from, to, replace, false)
}

func (b ahpBoxes) CopyFile(ctx context.Context, sandboxID, from, to string, replace bool) error {
	return b.transferFile(ctx, sandboxID, from, to, replace, true)
}

func (b ahpBoxes) transferFile(ctx context.Context, sandboxID, from, to string, replace, copyIt bool) error {
	app, client, projectID, err := b.filesAt(sandboxID)
	if err != nil {
		return err
	}
	req := &apiclientgen.SandboxFileTransferRequest{From: from, To: to, Replace: apiclientgen.NewOptBool(replace)}
	var res any
	if copyIt {
		res, err = client.CopySandboxFile(ctx, req, apiclientgen.CopySandboxFileParams{ProjectId: projectID, SandboxId: sandboxID})
	} else {
		res, err = client.MoveSandboxFile(ctx, req, apiclientgen.MoveSandboxFileParams{ProjectId: projectID, SandboxId: sandboxID})
	}
	if err != nil {
		return sandboxFileCallError(err)
	}
	// 409 is a destination that is there, not to be replaced.
	_, err = sandboxFileAnswer[apiclientgen.SandboxFileStat](res, to, errSandboxFileExists, app.sandboxFileThere(ctx, client, projectID, sandboxID, to))
	return err
}

func ahpFileInfo(stat apiclientgen.SandboxFileStat) ahphost.FileInfo {
	return ahphost.FileInfo{
		Path:       stat.Path,
		Name:       stat.Name,
		Type:       ahphost.FileType(stat.Type),
		Size:       stat.Size,
		ModTime:    stat.ModifiedAt,
		Version:    stat.Version,
		LinkTarget: stat.LinkTarget.Or(""),
	}
}
