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
		// Name and Target are only ever drawn, so they are made safe to
		// draw here, as every box-written string the console shows is;
		// Path is the identifier sent back, and stays the box's own.
		entries = append(entries, tui.FileEntry{
			Name:   terminalSafe(e.Name),
			Path:   e.Path,
			Dir:    e.Type == apiclientgen.SandboxFileTypeDirectory,
			Link:   e.Type == apiclientgen.SandboxFileTypeSymlink,
			Target: terminalSafe(e.LinkTarget.Or("")),
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
	// cp's copy replaces a file and merges into a directory; a drop is one
	// gesture with no confirmation, so it never lands on something already
	// there (ADR 26-11-10-203), as a move or copy inside the box never does.
	for _, p := range local {
		name := filepath.Base(p)
		_, err := copyStat(ctx, d.client, d.projectID, cpOperand{remote: true, reference: sandboxID, path: path.Join(dir, name)}, false)
		if err == nil {
			return fmt.Errorf("%q is already in %q", terminalSafe(name), terminalSafe(dir))
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
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
// downloads directory, under a name nothing there has yet: the copy
// `discobox cp -r BOX:PATH LOCAL` makes, into a name claimed first.
//
// Everything the discobox decides — the name, and for a directory every name
// inside it — lands below that directory and nowhere else: the name is
// claimed through an os.Root opened on it, and the copy writes through one
// pinned to what was claimed (copyarchive), so neither a name nor a link can
// reach past it.
func (d *apiDataSource) DownloadFile(ctx context.Context, sandboxID, remote string) (string, error) {
	name := path.Base(remote)
	// The name is the discobox's; it becomes a file name here only if it is
	// one, and never a path that climbs out of the directory.
	if err := copyarchive.Name(name); err != nil || strings.Contains(name, "/") {
		return "", fmt.Errorf("%s cannot be a file name on this machine", terminalSafe(name))
	}
	source := cpOperand{remote: true, reference: sandboxID, path: remote}
	stat, err := copyStat(ctx, d.client, d.projectID, source, true)
	if err != nil {
		return "", err
	}
	isDir := stat.Type == apiclientgen.SandboxFileTypeDirectory
	if !isDir && stat.Type != apiclientgen.SandboxFileTypeFile {
		return "", fmt.Errorf("special files cannot be downloaded: %s", terminalSafe(remote))
	}
	dir, err := downloadsDir()
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	claimed, err := claimLocalName(root, name, isDir, fs.FileMode(stat.Mode)&0o777)
	if err != nil {
		return "", fmt.Errorf("%s: %w", dir, err)
	}
	dest := cpOperand{path: filepath.Join(dir, claimed)}
	if isDir {
		err = d.app.copyDirectory(ctx, d.projectID, source, dest, false)
	} else {
		err = d.app.copyFile(ctx, d.client, d.projectID, source, dest, stat, false)
	}
	if err != nil {
		// What was claimed is this download's alone, whole or partial.
		_ = root.RemoveAll(claimed)
		return "", fmt.Errorf("copy %s: %w", terminalSafe(remote), err)
	}
	return dest.path, nil
}

// downloadsDir is where a download lands: the XDG download directory when
// one is named, else ~/Downloads, made when there is none yet. Never the home
// directory itself, where a box's file named .bashrc or .profile would be one
// the shell runs.
func downloadsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory to download beneath: %w", err)
	}
	if dir := os.Getenv("XDG_DOWNLOAD_DIR"); filepath.IsAbs(dir) && filepath.Clean(dir) != filepath.Clean(home) {
		return dir, nil
	}
	dir := filepath.Join(home, "Downloads")
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	return dir, nil
}

// claimLocalName creates name in root, or "name (2).ext" and on when that is
// taken, the way a browser names a second download of the same file: an
// empty file or directory made only if nothing is there, so the copy that
// fills it never lands on something that already was.
func claimLocalName(root *os.Root, name string, dir bool, mode fs.FileMode) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	if stem == "" || dir {
		// A dotfile is all extension, and a directory has none; either is
		// numbered after its whole name.
		stem, ext = name, ""
	}
	for n := 1; n < 1000; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		var err error
		if dir {
			err = root.Mkdir(candidate, 0o777)
		} else {
			var f *os.File
			if f, err = root.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode); err == nil {
				err = f.Close()
			}
		}
		switch {
		case err == nil:
			return candidate, nil
		case !errors.Is(err, fs.ErrExist):
			return "", err
		}
	}
	return "", fmt.Errorf("no free name for %s", terminalSafe(name))
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
