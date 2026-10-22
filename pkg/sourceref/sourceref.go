// Package sourceref is the one way a source is named: a directory, a Git URL
// or a GitHub repository written short, with a ref and a path inside it,
// `LOCATION[@REF][//PATH]` (ADR 26-10-15-664). It parses that notation, tells
// two references to one source apart from two sources, and spells a reference
// for a person to read and type back.
package sourceref

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Kind is what a reference's location names.
type Kind uint8

const (
	// Directory is a directory on this machine.
	Directory Kind = iota + 1
	// Git is a Git URL.
	Git
	// Shorthand is a GitHub repository written `owner/repo` or
	// `github.com/owner/repo`, which is also a relative path: Resolve says
	// which it is.
	Shorthand
)

// Ref is a parsed source reference.
type Ref struct {
	Kind Kind
	// Location is the directory, the Git URL, or the shorthand, as written
	// until Resolve settles it: a directory absolute, a shorthand the Git
	// URL it stands for.
	Location string
	// Ref is the branch, tag or commit named after `@`; empty for none.
	Ref string
	// Path is the clean, relative, slash-separated directory named after
	// `//`; empty for the source's root.
	Path string
}

// Parse reads `LOCATION[@REF][//PATH]`. It reads no disk, so a shorthand
// stays one until Resolve.
//
// PATH starts at the first `//` after the URL's `://`, if it has one: Git
// refuses consecutive slashes in a ref name, and a location has none after
// its scheme, so PATH may hold `@` and `:`. REF is what follows the last `@`
// past the URL's host, so a user in a URL or an scp-like `git@host:` is never
// read as one.
func Parse(value string) (Ref, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return Ref{}, errors.New("name a directory, a Git URL, or owner/repo")
	}
	location, dir, hasPath := splitPath(value)
	location, ref := splitRef(location)
	if location == "" {
		return Ref{}, fmt.Errorf("%q names no directory or repository", value)
	}
	r := Ref{Location: location, Ref: ref}
	if hasPath {
		if err := ValidatePath(dir); err != nil {
			return Ref{}, err
		}
		if dir != "." {
			r.Path = dir
		}
	}
	switch {
	case IsGitURL(location):
		r.Kind = Git
	case gitHubShorthand(location) != "":
		r.Kind = Shorthand
	default:
		r.Kind = Directory
	}
	return r, nil
}

// splitPath cuts `//PATH` off a value. A `//` that starts the value is a
// path's own (a POSIX path may begin with one), not the separator.
func splitPath(value string) (location, dir string, ok bool) {
	start := 0
	if i := strings.Index(value, "://"); i >= 0 {
		start = i + len("://")
	}
	i := strings.Index(value[start:], "//")
	if i < 0 || start+i == 0 {
		return value, "", false
	}
	return value[:start+i], value[start+i+len("//"):], true
}

// splitRef cuts `@REF` off a location. Only an `@` past the URL's host — or,
// for an scp-like URL, past its colon — can start one.
func splitRef(location string) (string, string) {
	floor := 0
	if i := strings.Index(location, "://"); i >= 0 {
		slash := strings.IndexByte(location[i+len("://"):], '/')
		if slash < 0 {
			return location, ""
		}
		floor = i + len("://") + slash
	} else if host, _, ok := scpLike(location); ok {
		floor = len(host)
	}
	at := strings.LastIndex(location, "@")
	if at <= floor || at == len(location)-1 {
		return location, ""
	}
	return location[:at], location[at+1:]
}

// scpLike splits git's scp-like `user@host:path`. The user is required: a
// bare `host:path` is as often a directory with a colon in its name.
func scpLike(value string) (host, rest string, ok bool) {
	host, rest, ok = strings.Cut(value, ":")
	if !ok || strings.HasPrefix(value, ".") || strings.ContainsAny(host, `/\`) {
		return "", "", false
	}
	user, name, hasUser := strings.Cut(host, "@")
	if !hasUser || user == "" || len(name) < 2 {
		return "", "", false
	}
	return host, rest, true
}

// IsGitURL reports whether value, with no @REF or //PATH, is a Git URL: a
// scheme with a host, a file URL, or the scp-like `user@host:path`.
func IsGitURL(value string) bool {
	if strings.Contains(value, "://") {
		u, err := url.Parse(value)
		return err == nil && u.Scheme != "" && (u.Host != "" || u.Scheme == "file")
	}
	_, _, ok := scpLike(value)
	return ok
}

// ValidatePath refuses a PATH that is not a clean relative path inside its
// source: slash-separated whatever the platform, no `..` segment, no control
// character. `.` is the root.
func ValidatePath(dir string) error {
	switch {
	case dir == "":
		return errors.New("// needs a path after it; the root is \".\"")
	case dir == ".":
		return nil
	case strings.Contains(dir, `\`):
		return fmt.Errorf("path %q is not slash-separated", dir)
	case path.IsAbs(dir), path.Clean(dir) != dir, dir == "..", strings.HasPrefix(dir, "../"):
		return fmt.Errorf("path %q is not a clean path inside the source", dir)
	}
	for _, r := range dir {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path %q holds the control character %q", dir, r)
		}
	}
	return nil
}

// Resolve settles what the reference names on this machine, against base (the
// working directory when empty): a shorthand that is an existing path is that
// directory, and any other the GitHub URL it stands for; a directory has `~`
// expanded and is made absolute. A Git URL is left as written.
func (r Ref) Resolve(base string) (Ref, error) {
	if r.Kind == Shorthand {
		if _, err := os.Lstat(join(base, r.Location)); errors.Is(err, os.ErrNotExist) {
			return r.Expand(), nil
		}
		r.Kind = Directory
	}
	if r.Kind != Directory {
		return r, nil
	}
	// The directory wins here too: a directory whose own name holds `@` or
	// `//` — an account in a synced folder's path — is that directory, not
	// one at a ref or a path.
	if whole := r.String(); whole != r.Location {
		if info, err := os.Stat(join(base, expandHome(whole))); err == nil && info.IsDir() {
			r.Location, r.Ref, r.Path = whole, "", ""
		}
	}
	dir := expandHome(r.Location)
	if dir == "" {
		return Ref{}, fmt.Errorf("resolve %s: no home directory", r.Location)
	}
	abs, err := filepath.Abs(join(base, dir))
	if err != nil {
		return Ref{}, fmt.Errorf("resolve %s: %w", r.Location, err)
	}
	r.Location = abs
	return r, nil
}

// Expand is a shorthand as the GitHub URL it stands for, whatever is on this
// machine's disk, for a place that never names a directory. Any other
// reference comes back as it is.
func (r Ref) Expand() Ref {
	if r.Kind == Shorthand {
		r.Kind, r.Location = Git, "https://github.com/"+gitHubShorthand(r.Location)+".git"
	}
	return r
}

// expandHome spells a `~`-prefixed directory from the home directory, and is
// empty when there is none to spell it from. Anything else comes back as it
// is.
func expandHome(dir string) string {
	if dir != "~" && !strings.HasPrefix(dir, "~/") && !strings.HasPrefix(dir, "~"+string(filepath.Separator)) {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, dir[1:])
}

func join(base, dir string) string {
	if base == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(base, dir)
}

// String is the reference as a source argument takes it.
func (r Ref) String() string {
	return r.Location + r.suffix()
}

func (r Ref) suffix() string {
	s := ""
	if r.Ref != "" {
		s += "@" + r.Ref
	}
	if r.Path != "" {
		s += "//" + r.Path
	}
	return s
}

// Key tells one source from another: two references with one key name the
// same repository or directory, at the same ref and path. A GitHub
// repository is keyed by its lower-cased `github.com/owner/repo`, since
// GitHub does not tell names apart by case; another remote by
// NormalizeRemote, so its HTTPS and SSH spellings are one; a directory by its
// path. Resolve a reference before keying it, or a relative directory is
// keyed as written.
func (r Ref) Key() string {
	return r.locationKey() + r.suffix()
}

// RepositoryKey is Key without @REF and //PATH: the repository or directory
// alone, which one checkout of it is of.
func (r Ref) RepositoryKey() string {
	return r.locationKey()
}

// CloneKey is Key with the transport the location is reached over: two
// references with one CloneKey would be fetched alike, so a list of things
// to cut from may offer them as one without running a URL nobody chose. ssh
// and https name one repository with one Key, but carry different
// credentials, and http and git:// carry none of a transport's protection.
func (r Ref) CloneKey() string {
	return r.transport() + " " + r.Key()
}

// transport is how the location is reached: the URL's scheme, with ssh's
// other spellings and the scp-like form as ssh, and nothing for a directory.
// A shorthand is reached over https, which is what it stands for.
func (r Ref) transport() string {
	switch r.Kind {
	case Shorthand:
		return "https"
	case Git:
		scheme, _, ok := strings.Cut(r.Location, "://")
		if !ok {
			return "ssh"
		}
		switch scheme = strings.ToLower(scheme); scheme {
		case "git+ssh", "ssh+git":
			return "ssh"
		default:
			return scheme
		}
	}
	return ""
}

func (r Ref) locationKey() string {
	if gh := r.GitHub(); gh != "" {
		return "github.com/" + strings.ToLower(gh)
	}
	switch r.Kind {
	case Git:
		if normalized := NormalizeRemote(r.Location); normalized != "" {
			return normalized
		}
		return r.Location
	default:
		return filepath.Clean(r.Location)
	}
}

// GitHub is the GitHub repository the reference names, `owner/repo` as
// written, or empty when it names none.
func (r Ref) GitHub() string {
	switch r.Kind {
	case Shorthand:
		return gitHubShorthand(r.Location)
	case Git:
		return strings.TrimPrefix(GitHubURL(r.Location), "https://github.com/")
	}
	return ""
}

// Display is the reference for a person to read, and is itself a reference
// with the same CloneKey: a GitHub repository reached over https as
// `owner/repo`, which is what that shorthand stands for; any other remote as
// its URL without `.git`, so its transport stays on screen; a directory under
// home, the home directory the caller names (empty for none), from `~`.
func (r Ref) Display(home string) string {
	if gh := r.GitHub(); gh != "" && r.transport() == "https" {
		return gh + r.suffix()
	}
	switch r.Kind {
	case Git:
		return strings.TrimSuffix(strings.TrimRight(r.Location, "/"), ".git") + r.suffix()
	default:
		return homeRelative(r.Location, home) + r.suffix()
	}
}

// Display is Parse(value).Display(home), or value itself when it does not
// parse: a listing shows what it was given rather than nothing.
func Display(value, home string) string {
	r, err := Parse(value)
	if err != nil {
		return value
	}
	return r.Display(home)
}

// Name is the last element of the location — a repository's name without
// `.git`, a directory's base name — which is what a source is called when
// nothing names it.
func (r Ref) Name() string {
	location := strings.TrimRight(r.Location, `/\`)
	if r.Kind == Git {
		if u, err := url.Parse(location); err == nil && u.Scheme != "" && u.Path != "" {
			location = u.Path
		} else if _, rest, ok := scpLike(location); ok {
			location = rest
		}
		return strings.TrimSuffix(path.Base(strings.TrimRight(location, "/")), ".git")
	}
	return filepath.Base(location)
}

// homeRelative spells an absolute directory under home from `~`. Anything
// else is left as it is.
func homeRelative(dir, home string) string {
	if home == "" || !filepath.IsAbs(dir) {
		return dir
	}
	rel, err := filepath.Rel(home, dir)
	switch {
	case err != nil, rel == "..", strings.HasPrefix(rel, ".."+string(filepath.Separator)):
		return dir
	case rel == ".":
		return "~"
	}
	return "~" + string(filepath.Separator) + rel
}

// gitHubShorthand is `owner/repo` for a location written `owner/repo` or
// `github.com/owner/repo`, either with `.git` or without, and empty for any
// other.
func gitHubShorthand(location string) string {
	parts := strings.Split(location, "/")
	if len(parts) == 3 && parts[0] == "github.com" {
		parts = parts[1:]
	}
	if len(parts) != 2 {
		return ""
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !gitHubOwner(owner) || !gitHubRepository(repo) {
		return ""
	}
	return owner + "/" + repo
}

// GitHubURL is the web address of the GitHub repository a Git remote names,
// `https://github.com/owner/repo`, or empty when it names something else.
// Every spelling Git takes for one counts — https, ssh:// and git:// URLs and
// the scp-like `git@github.com:owner/repo.git` — since which of them a source
// was written in says how it is reached, not what it is.
func GitHubURL(remote string) string {
	remote = strings.TrimSpace(remote)
	var host, repoPath string
	if strings.Contains(remote, "://") {
		// Only the transports GitHub serves: a file:// URL names a path on
		// this machine whatever it spells as its host.
		u, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		switch u.Scheme {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return ""
		}
		host, repoPath = u.Hostname(), u.Path
	} else {
		var ok bool
		host, repoPath, ok = strings.Cut(remote, ":")
		if !ok {
			return ""
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	}
	if host = strings.ToLower(host); host != "github.com" && host != "www.github.com" {
		return ""
	}
	parts := strings.Split(strings.Trim(repoPath, "/"), "/")
	if len(parts) != 2 {
		return ""
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")
	if !gitHubOwner(owner) || !gitHubRepository(repo) {
		return ""
	}
	return "https://github.com/" + owner + "/" + repo
}

// GitHubKey is what tells one GitHub repository from another, for a remote
// GitHubURL recognizes: its `owner/repo`, lower-cased because GitHub does not
// tell names apart by case. Empty for anything else.
func GitHubKey(remote string) string {
	return strings.ToLower(strings.TrimPrefix(GitHubURL(remote), "https://github.com/"))
}

// gitHubOwner reports whether name can be a GitHub user or organization:
// letters, digits and single hyphens, never at either end.
func gitHubOwner(name string) bool {
	if name == "" || strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") || strings.Contains(name, "--") {
		return false
	}
	for _, r := range name {
		if !isASCIIAlphanumeric(r) && r != '-' {
			return false
		}
	}
	return true
}

// gitHubRepository reports whether name can be a GitHub repository: letters,
// digits, '-', '_' and '.', but not a path's own "." or "..".
func gitHubRepository(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !isASCIIAlphanumeric(r) && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func isASCIIAlphanumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// defaultPorts are the ports a remote's scheme implies, which a URL may spell
// out or leave off.
var defaultPorts = map[string]string{"https": "443", "http": "80", "ssh": "22", "git": "9418"}

// NormalizeRemote reduces a remote's URL to its host and path — lower-cased
// host, no scheme, user, default port, trailing slash or ".git" — so the
// HTTPS and SSH spellings of one repository are one remote. It is empty for
// anything that is not a network remote.
func NormalizeRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.ContainsAny(remote, " \t\r\n\x00") {
		return ""
	}
	var host, port, repository string
	if strings.Contains(remote, "://") {
		parsed, err := url.Parse(remote)
		if err != nil {
			return ""
		}
		scheme := strings.ToLower(parsed.Scheme)
		// git+ssh and ssh+git are SSH by another name.
		if scheme == "git+ssh" || scheme == "ssh+git" {
			scheme = "ssh"
		}
		implied, ok := defaultPorts[scheme]
		if !ok {
			return ""
		}
		host, port, repository = parsed.Hostname(), parsed.Port(), parsed.Path
		if port == implied {
			port = ""
		}
	} else {
		// git's scp-like form, [user@]host:path. A colon after the first
		// slash is part of a path, and a one-letter host is a Windows drive.
		colon := strings.IndexByte(remote, ':')
		if colon <= 0 || strings.ContainsAny(remote[:colon], `/\`) {
			return ""
		}
		host, repository = remote[:colon], remote[colon+1:]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		if len(host) < 2 || strings.HasPrefix(repository, `\`) {
			return ""
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	repository = strings.TrimSuffix(strings.Trim(path.Clean("/"+repository), "/"), ".git")
	repository = strings.TrimRight(repository, "/")
	if host == "" || repository == "" || repository == "." {
		return ""
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + repository
}
