package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The launcher keeps the prompt you were typing when the window closed, so the
// next window in that folder opens holding it. Like the picker's memory it is
// derived convenience state, not configuration, so it lives in the CLI's state
// directory (statedir.go) beside the rest of it and is always best-effort: a
// missing, unreadable or corrupt file means the window opens empty, which is
// where it used to open every time.

// promptDraftsFile is the state file, relative to the CLI's state directory.
const promptDraftsFile = "prompt-drafts.json"

// promptDraftLimit bounds the file the way the picker's does, so a machine
// that has been through a lot of checkouts does not carry an entry for each of
// them forever.
const promptDraftLimit = 50

// promptDraftMax is the most of one prompt that is kept. A composer holds
// whatever is pasted into it, and a state file is not where a megabyte of
// pasted log belongs; the cut is far past anything anybody types.
const promptDraftMax = 64 << 10

type promptDraft struct {
	Text string `json:"text"`
	// At is when it was written, and is what trimming sorts on.
	At time.Time `json:"at"`
}

func promptDraftsPath() string {
	return filepath.Join(cliStateDir(), promptDraftsFile)
}

func loadPromptDrafts() map[string]promptDraft {
	data, err := os.ReadFile(promptDraftsPath())
	if err != nil {
		return nil
	}
	var drafts map[string]promptDraft
	if err := json.Unmarshal(data, &drafts); err != nil {
		return nil
	}
	return drafts
}

// promptDraftFor returns the prompt left in folder, or "" when there is none.
func promptDraftFor(folder string) string {
	if strings.TrimSpace(folder) == "" {
		return ""
	}
	return loadPromptDrafts()[folder].Text
}

// savePromptDraft records prompt as the draft for folder. An empty prompt drops
// the entry rather than storing nothing under it: the draft is gone, and a file
// full of empty strings is a file that outgrows its limit remembering nothing.
func savePromptDraft(folder, prompt string) error {
	if strings.TrimSpace(folder) == "" {
		return nil
	}
	drafts := loadPromptDrafts()
	if prompt == "" {
		if _, ok := drafts[folder]; !ok {
			return nil
		}
		delete(drafts, folder)
	} else {
		if len(prompt) > promptDraftMax {
			// On a rune boundary: the cut is arbitrary, and half a character
			// is not what was typed there.
			prompt = strings.ToValidUTF8(prompt[:promptDraftMax], "")
		}
		if drafts == nil {
			drafts = map[string]promptDraft{}
		}
		drafts[folder] = promptDraft{Text: prompt, At: time.Now().UTC()}
	}
	trimPromptDrafts(drafts)
	return writeStateFile(promptDraftsPath(), drafts)
}

func trimPromptDrafts(drafts map[string]promptDraft) {
	if len(drafts) <= promptDraftLimit {
		return
	}
	keys := make([]string, 0, len(drafts))
	for key := range drafts {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return drafts[keys[j]].At.Before(drafts[keys[i]].At) })
	for _, key := range keys[promptDraftLimit:] {
		delete(drafts, key)
	}
}
