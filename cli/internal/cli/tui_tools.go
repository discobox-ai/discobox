package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/tools"
)

// A tool's configuration lives on this machine, not in the project and not in
// the discobox: it is how *you* like your editor, which is a fact about you
// rather than about the repository or about any one box (ADR 26-08-27-302 on tool
// sessions).
//
// So there is nothing here that talks to the control plane. The copy is a file
// under the user's config directory, the discobox gets it through one exec that
// writes it only where there is nothing already, and editing it is $EDITOR on a
// real path — which means it is also editable by anything else you point at
// that path, dotfile managers included.

// toolConfigDir is where every tool's files are kept: the platform's config
// directory, which is ~/.config on Linux and macOS (honoring XDG_CONFIG_HOME)
// and %AppData% on Windows.
//
// The config directory rather than the state directory statedir.go resolves,
// and for the opposite reason: state is derived, per-machine and disposable —
// a generated ssh_config means nothing on another machine — while this is
// authored, is the only copy, and is exactly the kind of file people back up
// and carry with them.
func toolConfigDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("cannot find a config directory: %w", err)
	}
	return filepath.Join(dir, "discobox", "tools"), nil
}

// ToolFilePath is where one tool file's copy lives, whether or not it is there
// yet. Empty when there is no config directory to resolve it against, which the
// picker draws as simply not saying.
func (d *apiDataSource) ToolFilePath(file tui.ToolFile) string {
	path, err := toolFilePath(file.Tool, file.Name)
	if err != nil {
		return ""
	}
	return path
}

// toolFilePath is where one tool file's copy lives on this machine: under the
// user's tools directory, in a directory named for the tool. For a tool the
// user declared, that is the default beside the declaration itself.
func toolFilePath(tool, name string) (string, error) {
	// Both halves come from declarations, some of them a discobox's, and are
	// joined into a path on this machine. Checked here as well as where they
	// were read, because this is the write that a name full of separators
	// would carry out of the config directory.
	if !tools.PlainFileName(tool) || !tools.PlainFileName(name) {
		return "", fmt.Errorf("tool file %q of %q is not a plain file name", name, tool)
	}
	dir, err := toolConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, tool, name), nil
}

// ensureToolFile reads the local copy, creating it from the tool's default when
// there is none, and returns the path and the content.
//
// Created on first read rather than on first edit, so that a tool run before it
// was ever configured still carries the default in — and so that the first edit
// opens on that default rather than on an empty buffer.
func ensureToolFile(tool string, file tools.File) (string, string, error) {
	path, err := toolFilePath(tool, file.Name)
	if err != nil {
		return "", "", fmt.Errorf("cannot find a config directory for %s: %w", file.Name, err)
	}
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		return path, string(content), nil
	case !os.IsNotExist(err):
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(path, []byte(file.Default), 0o600); err != nil {
		return "", "", err
	}
	return path, file.Default, nil
}

// EditToolFile opens the local copy in the user's editor and reports whether
// what came back differs.
//
// The real file is edited in place rather than a temp copy of it, which is what
// the harness files get: there the file lives on the server and the temp file is
// the only way to hand it to an editor, while this one is already a path on this
// disk. Editing it directly means an editor that reopens where you left off, a
// $VISUAL of "code --wait" that opens the actual file in your actual project
// window, and a crash that loses nothing.
func (d *apiDataSource) EditToolFile(ctx context.Context, file tui.ToolFile, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	path, before, err := ensureToolFile(file.Tool, tools.File{Name: file.Name, Home: file.Home, Default: file.Default})
	if err != nil {
		return false, err
	}

	argv := append(editorCommand(), path)
	//nolint:gosec // Launching the user's own $VISUAL/$EDITOR is the point.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = stdin
	if stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("editor %s: %w", argv[0], err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(after, []byte(before)), nil
}

// installToolFiles puts a tool's files into the sandbox, each one only where
// there is nothing at that path already.
//
// One exec per file, and the decision is made inside the sandbox rather than
// here: "is there one already" and "write it" have to be the same step, or two
// windows opening the same tool at once race to create it. The content rides as
// its own argv element, so there is no quoting to get wrong and no encoding to
// depend on — only sh, printf and mkdir, which is as portable as the inside of
// a discobox gets.
func (a *App) installToolFiles(ctx context.Context, projectID, sandboxID string, def tools.Definition) error {
	for _, file := range def.Files {
		home := strings.TrimPrefix(strings.TrimSpace(file.Home), "/")
		if home == "" {
			continue
		}
		_, content, err := ensureToolFile(def.ID, file)
		if err != nil {
			return err
		}
		command := []string{"sh", "-c", installToolFileScript, "sh", home, content}
		_, errOut, code, err := a.sandboxCommandOutput(ctx, projectID, sandboxID, "", command)
		if err != nil {
			return fmt.Errorf("install %s: %w", file.Name, err)
		}
		if code != 0 {
			detail := strings.TrimSpace(errOut)
			if detail == "" {
				detail = fmt.Sprintf("exit %d", code)
			}
			return fmt.Errorf("install %s: %s", file.Name, detail)
		}
	}
	return nil
}

// installToolFileScript writes $2 to $HOME/$1, and does nothing at all if
// something is already there.
//
// The destination is a fixed path under the home directory. A tool whose state
// has to be keyed on something only the discobox knows — fresh's trust
// decision, keyed on the working directory — records it in its own script
// instead (ADR 0125 §6).
//
// The test is an `if` rather than `[ -e "$p" ] && exit 0`: under `set -e` a
// failing AND-list is the list's own exit status, so the shell would leave with
// 1 in exactly the case that is supposed to be normal.
const installToolFileScript = `set -e
p="$HOME/$1"
if [ ! -e "$p" ]; then
	mkdir -p "$(dirname "$p")"
	printf %s "$2" > "$p"
fi`
