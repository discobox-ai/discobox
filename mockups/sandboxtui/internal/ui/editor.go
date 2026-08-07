package ui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// A prompt long enough to be worth writing carefully is worth writing in the
// editor you already know. Alt-E hands the buffer to $EDITOR and takes back
// whatever it saved.
//
// This is the one thing in the mockup that is not a mock: it really does run
// your editor, because the interaction cannot be judged without doing it.

// editorDoneMsg carries the temp file back once the editor exits.
type editorDoneMsg struct {
	path string
	err  error
}

// editPrompt writes the buffer to a temp file and hands the terminal to the
// editor. Bubble Tea suspends itself for the duration and redraws afterwards,
// which is why the window is inline: nothing has to be restored.
func editPrompt(text string) tea.Cmd {
	file, err := os.CreateTemp("", "disco-prompt-*.md")
	if err != nil {
		return failed("cannot write the prompt out: %v", err)
	}
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		os.Remove(file.Name())
		return failed("cannot write the prompt out: %v", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return failed("cannot write the prompt out: %v", err)
	}

	command, err := editorCommand(file.Name())
	if err != nil {
		os.Remove(file.Name())
		return failed("%v", err)
	}
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return editorDoneMsg{path: file.Name(), err: err}
	})
}

// editorCommand resolves the editor the way every tool that shells out to one
// does: $VISUAL, then $EDITOR, then vi.
//
// The variable may carry arguments — "code -w", "emacsclient -nw" — so it is
// split on spaces rather than treated as a bare program name. That is not a
// shell, and an editor whose path contains a space needs $VISUAL to be a
// wrapper script, which is the same deal git offers.
func editorCommand(path string) (*exec.Cmd, error) {
	editor := firstSet("VISUAL", "EDITOR")
	if editor == "" {
		editor = "vi"
	}
	parts := strings.Fields(editor)
	if len(parts) == 0 {
		return nil, fmt.Errorf("$EDITOR is empty")
	}
	program, err := exec.LookPath(parts[0])
	if err != nil {
		return nil, fmt.Errorf("cannot run %q: %v", parts[0], err)
	}
	return exec.Command(program, append(parts[1:], path)...), nil
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
