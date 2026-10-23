package tui

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

// folder is one of the filter's choices: the key of the discoboxes in it, what
// it reads as, the source a create from it cuts from,
// and whether it is this machine's — in which case this machine's discoboxes
// with no source are in it too, beside the directory's own.
type folder struct {
	key    string
	label  string
	source string
	local  bool
}

// allFolders is the choice that is not a place: every sandbox in the project,
// wherever it was started. It is last among the folders rather than first,
// because the folder you are standing in is the one you almost always want.
const allFolders = "all folders"

// everyFolder is allFolders as a choice: no key, so nothing is filtered out.
var everyFolder = folder{label: allFolders}

// holds reports whether a discobox is in the folder: its folder is this one,
// or — in a folder of this machine's — it is filed under this machine's own
// key, where its discoboxes with no source are. Every discobox is in the
// folder with no key.
func (f folder) holds(s Sandbox, session Session) bool {
	switch {
	case f.key == "":
		return true
	case s.folderKey(session) == f.key:
		return true
	case s.OriginKey == f.key:
		// A view saved on an origin key whose discoboxes have since resolved
		// to a repository still lists them, until the header is moved.
		return true
	default:
		return f.local && session.HostKey != "" && s.OriginKey == session.HostKey
	}
}

// folder is the window's own folder, and cuts from the window's own source. A
// window whose source resolves to a GitHub repository now — through the
// remote its checked-out branch tracks, as a create would record it — is in
// that repository's folder, named as the repository with no directory or
// branch: the header names the folder, and the Source row in the run options
// names what is cut. Otherwise it is the origin key's, named the way the
// header spells that source, which is the one place a branch is shown, since
// it belongs to the directory the window is running in and means nothing next
// to a folder somewhere else. It is this machine's, so its discoboxes with no
// source are in it.
func (s Session) folder() folder {
	if s.RepositoryKey != "" {
		return folder{key: s.RepositoryKey, label: repositoryLabel(s.RepositoryKey), source: s.Directory, local: true}
	}
	return folder{key: s.OriginKey, label: s.sourceLabel(), source: s.Directory, local: true}
}

// repositoryLabel is how a GitHub repository's folder reads, from its key
// (Sandbox.RepositoryKey): `owner/repo on github`.
func repositoryLabel(key string) string {
	return key + " on github"
}
