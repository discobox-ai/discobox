package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A prompt long enough to be worth writing carefully is worth writing in the
// editor you already know. The edit in editor gesture (Alt-E, F2) hands the
// buffer to $EDITOR and takes back whatever it saved.

// editorDoneMsg carries the temp file back once the editor exits.
type editorDoneMsg struct {
	path string
	err  error
}

// editPrompt writes the buffer to a temp file and hands the terminal to the
// editor. Bubble Tea suspends itself for the duration — dropping out of the
// alternate screen, so the editor gets a screen of its own — and redraws when
// it exits.
func editPrompt(ctx context.Context, text string) tea.Cmd {
	return editText(ctx, text, "prompt", func(path string, err error) tea.Msg {
		return editorDoneMsg{path: path, err: err}
	})
}

// editText is that round trip for any text the window holds — the prompt, an
// issue comment — named in what it says when it fails. done is handed the temp
// file once the editor exits; reading it back, and removing it, is the
// caller's.
func editText(ctx context.Context, text, what string, done func(path string, err error) tea.Msg) tea.Cmd {
	file, err := os.CreateTemp("", "discobox-"+strings.ReplaceAll(what, " ", "-")+"-*.md")
	if err != nil {
		return failed("cannot write the %s out: %v", what, err)
	}
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		os.Remove(file.Name())
		return failed("cannot write the %s out: %v", what, err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return failed("cannot write the %s out: %v", what, err)
	}

	command, err := editorCommand(ctx, file.Name())
	if err != nil {
		os.Remove(file.Name())
		return failed("%v", err)
	}
	path := file.Name()
	return tea.ExecProcess(command, func(err error) tea.Msg { return done(path, err) })
}

// editorCommand resolves the editor the way every tool that shells out to one
// does: $VISUAL, then $EDITOR, then the platform's default.
//
// The variable may carry arguments — "code -w", "emacsclient -nw" — so it is
// split on spaces rather than treated as a bare program name. That is not a
// shell, and an editor whose path contains a space needs $VISUAL to be a
// wrapper script, which is the same deal git offers.
func editorCommand(ctx context.Context, path string) (*exec.Cmd, error) {
	editor := firstSet("VISUAL", "EDITOR")
	if editor == "" {
		editor = defaultEditor()
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return nil, fmt.Errorf("$EDITOR is empty")
	}
	program, err := exec.LookPath(parts[0])
	if err != nil {
		return nil, fmt.Errorf("cannot run %q: %w", parts[0], err)
	}
	// The context is the window's: quitting it should not leave an editor
	// holding the terminal.
	return exec.CommandContext(ctx, program, append(parts[1:], path)...), nil
}

// defaultEditor is what to run when neither variable is set. Windows has no
// vi, but Windows 11 ships edit.exe — a terminal editor that takes over the
// screen the way this code expects — with notepad as the backstop for builds
// without it.
func defaultEditor() string {
	if runtime.GOOS != "windows" {
		return "vi"
	}
	for _, candidate := range []string{"edit.exe", "notepad.exe"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate
		}
	}
	return "notepad.exe"
}

func firstSet(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func failed(format string, args ...any) tea.Cmd {
	return func() tea.Msg {
		return statusMsg{text: fmt.Sprintf(format, args...), err: true}
	}
}

// editorChords are the registry's edit in editor gesture's keys: the first is
// the one a hint names (editorLabel).
var editorChords = func() []string {
	g, ok := console.LookupGesture("edit in editor")
	if !ok || len(g.Bindings.TUI.Chords) == 0 {
		panic("tui: no edit in editor gesture in the registry")
	}
	return g.Bindings.TUI.Chords
}()

// editorLabel is the edit in editor gesture's first key as a hint spells it.
var editorLabel = keyLabel(editorChords[0])

// editorKey reports whether a key hands a field being written to the
// editor: the registry's edit in editor gesture, from the prompt and an issue
// tab's comment.
func editorKey(msg tea.KeyPressMsg) bool { return slices.Contains(editorChords, msg.String()) }
