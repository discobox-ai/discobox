package tui

import "github.com/discobox-ai/discobox/cli/internal/console"

// The folder is the place in the header's filter (filter.go): it says where
// the sandboxes on screen came from, and changing it changes which ones are on
// screen. It stands in for both a folder column — every row on screen shares
// the value, so a column repeating it says nothing — and a key toggling "only
// the ones started here", which would be the same filter with one of its
// choices missing.

// A folder is one of two things (ADR 26-10-14-585). A discobox whose source
// resolved to a GitHub repository is in that repository's folder, keyed by the
// repository (Sandbox.RepositoryKey) and named `owner/repo on github`: one
// folder for every clone, spelling of the URL and machine it was cut from. Any
// other discobox is in its origin key's folder (ADR 0111): a machine, and the
// directory or repository URL the discobox had its source from — or the
// machine alone for one with none — named by that source, so one such path on
// two machines is two folders.

// allFolders is the choice that is not a place: every sandbox in the project,
// wherever it was started. It is last among the folders rather than first,
// because the folder you are standing in is the one you almost always want.
const allFolders = "all folders"

// everyFolder is allFolders as a choice: no key, so nothing is filtered out.
var everyFolder = console.Folder{Label: allFolders}
