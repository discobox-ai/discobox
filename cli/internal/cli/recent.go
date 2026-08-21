package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The picker remembers the last thing it was pointed at so the next pick opens
// on it. It is best-effort like the rest of the CLI's state (see statedir.go):
// a missing, unreadable, or corrupt file just means the picker falls back to
// its normal ordering.

// recentSelectionsFile is the state file, relative to the CLI's state directory.
const recentSelectionsFile = "recent-selections.json"

// recentSelectionLimit bounds the file so a long-lived install does not
// accumulate an entry per project forever.
const recentSelectionLimit = 50

type recentSelection struct {
	ID string `json:"id"`
	// At is when the selection was made, and is what trimming sorts on.
	At time.Time `json:"at"`
}

func recentSelectionsPath() string {
	return filepath.Join(cliStateDir(), recentSelectionsFile)
}

func loadRecentSelections() map[string]recentSelection {
	data, err := os.ReadFile(recentSelectionsPath())
	if err != nil {
		return nil
	}
	var selections map[string]recentSelection
	if err := json.Unmarshal(data, &selections); err != nil {
		return nil
	}
	return selections
}

// recentSelection returns the ID last picked for key, or "" when there is none.
func lastSelection(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return loadRecentSelections()[key].ID
}

// rememberSelection records id as the latest pick for key. Failures are
// reported so callers can decide, but the picker deliberately ignores them:
// losing the memory is not a reason to fail the command the user ran.
func rememberSelection(key, id string) error {
	if strings.TrimSpace(key) == "" || strings.TrimSpace(id) == "" {
		return nil
	}
	selections := loadRecentSelections()
	if selections == nil {
		selections = map[string]recentSelection{}
	}
	selections[key] = recentSelection{ID: id, At: time.Now().UTC()}
	trimRecentSelections(selections)
	return writeStateFile(recentSelectionsPath(), selections)
}

func trimRecentSelections(selections map[string]recentSelection) {
	if len(selections) <= recentSelectionLimit {
		return
	}
	keys := make([]string, 0, len(selections))
	for key := range selections {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return selections[keys[j]].At.Before(selections[keys[i]].At) })
	for _, key := range keys[recentSelectionLimit:] {
		delete(selections, key)
	}
}
