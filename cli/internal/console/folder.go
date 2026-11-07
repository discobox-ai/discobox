package console

// Folder is one of the filter's choices: the key of the discoboxes in it, what
// it reads as, the source a create from it cuts from,
// and whether it is this machine's — in which case this machine's discoboxes
// with no source are in it too, beside the directory's own.
type Folder struct {
	Key    string
	Label  string
	Source string
	Local  bool
}

// Holds reports whether a discobox is in the folder: its folder is this one,
// or — in a folder of this machine's — it is filed under this machine's own
// key, where its discoboxes with no source are. Every discobox is in the
// folder with no key.
func (f Folder) Holds(s Sandbox, session Session) bool {
	switch {
	case f.Key == "":
		return true
	case s.FolderKey(session) == f.Key:
		return true
	case s.OriginKey == f.Key:
		// A view saved on an origin key whose discoboxes have since resolved
		// to a repository still lists them, until the header is moved.
		return true
	default:
		return f.Local && session.HostKey != "" && s.OriginKey == session.HostKey
	}
}

// Folder is the window's own folder, and cuts from the window's own source. A
// window whose source resolves to a GitHub repository now — through the
// remote its checked-out branch tracks, as a create would record it — is in
// that repository's folder, named as the repository with no directory or
// branch: the header names the folder, and the Source row in the run options
// names what is cut. Otherwise it is the origin key's, named the way the
// header spells that source, which is the one place a branch is shown, since
// it belongs to the directory the window is running in and means nothing next
// to a folder somewhere else. It is this machine's, so its discoboxes with no
// source are in it.
func (s Session) Folder() Folder {
	if s.RepositoryKey != "" {
		return Folder{Key: s.RepositoryKey, Label: RepositoryLabel(s.RepositoryKey), Source: s.Directory, Local: true}
	}
	return Folder{Key: s.OriginKey, Label: s.SourceLabel(), Source: s.Directory, Local: true}
}

// RepositoryLabel is how a GitHub repository's folder reads, from its key
// (Sandbox.RepositoryKey): `owner/repo on github`.
func RepositoryLabel(key string) string {
	return key + " on github"
}
