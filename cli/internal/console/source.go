package console

import "github.com/discobox-ai/discobox/pkg/sourceref"

// Source is one place the project's discoboxes have been cut from, as the
// Source row offers it.
type Source struct {
	// Value is what `run`'s SOURCE takes: the client directory, or the
	// repository URL.
	Value string
	// Remote marks a repository URL rather than a directory on a client.
	Remote bool
	// Directory is the directory a local Value names, without its ref or
	// path, as the data source resolved it; empty for a remote. It is not
	// split off Value here: a directory's own name may hold `@` or `//`,
	// which only its disk tells from a ref or a path (sourceref.Ref.Resolve).
	Directory string
	// OriginKey is where a discobox cut from Value on this machine is filed
	// (ADR 0111), which is the folder the list follows the source to.
	OriginKey string
	// RepositoryKey is the GitHub repository Value belongs to, keyed as
	// Sandbox.RepositoryKey is, and empty when it is not on GitHub: the
	// folder the list follows the source to is then the repository's.
	RepositoryKey string
}

// NoSourceChoice is how sourceNone reads, in the panel and on the chip strip
// both. It is the flag's own words rather than "(none)", which reads as a value
// that has not been filled in yet rather than as the answer it is.
const NoSourceChoice = "no source"

// SourceLabel is what a sandbox is cut from by default, in the spelling the
// header uses: the window's directory and the branch checked out there.
func (s Session) SourceLabel() string {
	if s.Branch == "" {
		return sourceref.Display(s.Directory, s.Home)
	}
	return sourceref.Display(s.Directory, s.Home) + " @ " + s.Branch
}
