package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// The console keeps three things across launches, one of each on this machine
// whatever directory a window is opened in (ADR 26-10-20-596): the prompt left
// unsent in the composer, the header's filters — server, folder, tags — and the
// source the last create was cut from. They are derived convenience state, kept
// in the CLI's state directory (statedir.go) and always best-effort: a missing,
// unreadable or corrupt file opens the window empty, on everything, cutting
// from where it runs, which is where it opens without one.

// consoleStateFile is the state file, relative to the CLI's state directory.
const consoleStateFile = "console.json"

// promptDraftMax is the most of one prompt that is kept. A composer holds
// whatever is pasted into it, and a state file is not where a megabyte of
// pasted log belongs; the cut is far past anything anybody types.
const promptDraftMax = 64 << 10

type consoleState struct {
	Draft      string      `json:"draft,omitempty"`
	View       consoleView `json:"view"`
	LastSource string      `json:"lastSource,omitempty"`
}

type consoleView struct {
	Server       string   `json:"server,omitempty"`
	FolderKey    string   `json:"folderKey,omitempty"`
	FolderLabel  string   `json:"folderLabel,omitempty"`
	FolderSource string   `json:"folderSource,omitempty"`
	FolderLocal  bool     `json:"folderLocal,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// consoleStateMu serializes this process's read-modify-writes of the file. The
// window saves the draft and the view from commands that run concurrently, and
// each rewrites the whole file: unserialized, one would put back the field the
// other had just changed. Two windows open at once still share one file: the
// last to write a field wins it, which is why a window closing with a prompt
// writes it again (tui's saveDraftNow).
var consoleStateMu sync.Mutex

func consoleStatePath() string {
	return filepath.Join(cliStateDir(), consoleStateFile)
}

func loadConsoleState() consoleState {
	var state consoleState
	data, err := os.ReadFile(consoleStatePath())
	if err != nil {
		return consoleState{}
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return consoleState{}
	}
	return state
}

// updateConsoleState changes one thing in the file and leaves the rest as it
// was read.
func updateConsoleState(change func(*consoleState)) error {
	consoleStateMu.Lock()
	defer consoleStateMu.Unlock()
	state := loadConsoleState()
	change(&state)
	return writeStateFile(consoleStatePath(), state)
}

// savePromptDraft records prompt as the draft. An empty prompt drops it.
func savePromptDraft(prompt string) error {
	if len(prompt) > promptDraftMax {
		// On a rune boundary: the cut is arbitrary, and half a character is
		// not what was typed there.
		prompt = strings.ToValidUTF8(prompt[:promptDraftMax], "")
	}
	return updateConsoleState(func(s *consoleState) { s.Draft = prompt })
}

// saveConsoleView records view as the header's filters. The zero view is the
// default, and is stored as nothing at all.
func saveConsoleView(view tui.ListView) error {
	return updateConsoleState(func(s *consoleState) {
		s.View = consoleView{
			Server:       view.Server,
			FolderKey:    view.FolderKey,
			FolderLabel:  view.FolderLabel,
			FolderSource: view.FolderSource,
			FolderLocal:  view.FolderLocal,
			Tags:         view.Tags,
		}
	})
}

// saveLastSource records the source the last create was cut from, as the
// console's Source row spelled it.
func saveLastSource(source string) error {
	return updateConsoleState(func(s *consoleState) { s.LastSource = source })
}

func (v consoleView) listView() tui.ListView {
	return tui.ListView{
		Server:       v.Server,
		FolderKey:    v.FolderKey,
		FolderLabel:  v.FolderLabel,
		FolderSource: v.FolderSource,
		FolderLocal:  v.FolderLocal,
		Tags:         v.Tags,
	}
}
