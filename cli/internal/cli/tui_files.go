package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/pkg/copyarchive"
)

// The console's files panel (tui/files.go) over the discobox's files API
// (ADR 26-10-22-499): a listing to draw, a move or copy for a drag inside the
// panel, and the copies `discobox cp` makes for a file dropped onto it from
// this machine and for one downloaded from it; and a short command run to
// its end, which is how the panel asks git about the files it shows.

// ListFiles lists one directory in the discobox.
func (d *apiDataSource) ListFiles(ctx context.Context, sandboxID, dir string) (string, []tui.FileEntry, error) {
	res, err := d.client.ListSandboxFiles(ctx, apiclientgen.ListSandboxFilesParams{ProjectId: d.projectID, SandboxId: sandboxID, Path: dir})
	if err != nil {
		return "", nil, sandboxFileCallError(err)
	}
	list, err := expectResponse[apiclientgen.SandboxFileList](res)
	if err != nil {
		// The discobox's own words, escaped before anything prints them.
		return "", nil, errors.New(terminalSafe(err.Error()))
	}
	entries := make([]tui.FileEntry, 0, len(list.Entries))
	for _, e := range list.Entries {
		entries = append(entries, tui.FileEntry{
			Name:   e.Name,
			Path:   e.Path,
			Dir:    e.Type == apiclientgen.SandboxFileTypeDirectory,
			Link:   e.Type == apiclientgen.SandboxFileTypeSymlink,
			Target: e.LinkTarget.Or(""),
			Size:   e.Size,
		})
	}
	return list.Path, entries, nil
}

// TransferFile moves or copies one path within the discobox, never over
// something already there.
func (d *apiDataSource) TransferFile(ctx context.Context, sandboxID, from, to string, duplicate bool) error {
	body := &apiclientgen.SandboxFileTransferRequest{From: from, To: to}
	var (
		res any
		err error
	)
	if duplicate {
		res, err = d.client.CopySandboxFile(ctx, body, apiclientgen.CopySandboxFileParams{ProjectId: d.projectID, SandboxId: sandboxID})
	} else {
		res, err = d.client.MoveSandboxFile(ctx, body, apiclientgen.MoveSandboxFileParams{ProjectId: d.projectID, SandboxId: sandboxID})
	}
	if err != nil {
		return sandboxFileCallError(err)
	}
	_, err = sandboxFileResponse(res)
	return err
}

// UploadFiles copies local files and directories into dir in the discobox:
// the operands `discobox cp -r LOCAL... BOX:DIR/` would be, through the same
// copy, so a directory goes over as one copy archive.
func (d *apiDataSource) UploadFiles(ctx context.Context, sandboxID string, local []string, dir string) error {
	if len(local) == 0 {
		return nil
	}
	operands := make([]cpOperand, 0, len(local)+1)
	for _, p := range local {
		operands = append(operands, cpOperand{path: p})
	}
	// The trailing slash makes the destination a directory that must exist,
	// so one file is copied under its own name rather than onto dir.
	dest := path.Clean(dir)
	if dest != "/" {
		dest += "/"
	}
	operands = append(operands, cpOperand{remote: true, reference: sandboxID, path: dest})
	return d.app.copyOperands(ctx, d.client, d.projectID, operands, copyOptions{recursive: true, quiet: true})
}

// DownloadFile copies one path out of the discobox into this machine's
// downloads directory, under a name nothing there has yet: the operands
// `discobox cp -r BOX:PATH LOCAL` would be, through the same copy.
func (d *apiDataSource) DownloadFile(ctx context.Context, sandboxID, remote string) (string, error) {
	name := path.Base(remote)
	// The name is the discobox's; it becomes a file name here only if it is
	// one, and never a path that climbs out of the directory.
	if err := copyarchive.Name(name); err != nil || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s cannot be a file name on this machine", terminalSafe(name))
	}
	dir, err := downloadsDir()
	if err != nil {
		return "", err
	}
	local, err := freeLocalName(dir, name)
	if err != nil {
		return "", err
	}
	operands := []cpOperand{{remote: true, reference: sandboxID, path: remote}, {path: local}}
	if err := d.app.copyOperands(ctx, d.client, d.projectID, operands, copyOptions{recursive: true, quiet: true}); err != nil {
		return "", err
	}
	return local, nil
}

// downloadsDir is where a download lands: the XDG download directory when
// one is named, else ~/Downloads when there is one, else the home directory.
func downloadsDir() (string, error) {
	if dir := os.Getenv("XDG_DOWNLOAD_DIR"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to download into: %w", err)
	}
	if info, err := os.Stat(filepath.Join(home, "Downloads")); err == nil && info.IsDir() {
		return filepath.Join(home, "Downloads"), nil
	}
	return home, nil
}

// freeLocalName is name in dir, or "name (2).ext" and on when that is
// taken, the way a browser names a second download of the same file.
func freeLocalName(dir, name string) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" {
		// A dotfile is all extension; it is numbered after, not before, it.
		stem, ext = name, ""
	}
	for n := 1; n < 1000; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		local := filepath.Join(dir, candidate)
		if _, err := os.Lstat(local); errors.Is(err, fs.ErrNotExist) {
			return local, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %s in %s", name, dir)
}

// RunCommand runs one command in the discobox to its end and reads what it
// wrote; sandboxCommandOutput removes the session after.
func (d *apiDataSource) RunCommand(ctx context.Context, sandboxID, workdir string, command []string) (tui.CommandResult, error) {
	stdout, stderr, code, err := d.app.sandboxCommandOutput(ctx, d.projectID, sandboxID, workdir, command)
	if err != nil {
		return tui.CommandResult{}, err
	}
	return tui.CommandResult{Stdout: stdout, Stderr: stderr, ExitCode: code}, nil
}
