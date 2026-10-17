package sandboxcreate

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sourceref"
	"github.com/discobox-ai/x/gitutil"
)

// A repository names the other repositories it is worked on with in its
// settings (sandboxconfig.SettingsFile, or the sources.json it replaces): a
// JSON object of name to Git URL, optionally with an @REF suffix.
//
//	{"sources": {"foo": "https://github.com/acme/foo", "bar": "git@github.com:acme/bar@main"}}
//
// They are read here, on the client, and not by the pool or the server:
// deciding what a declared source resolves to means looking at this machine's
// disk for a checkout of it, which nothing else in the system can see. A
// remote primary source's are read from its remote, at the commit it is
// pinned to (readRemoteSettings). The pool agent reads the same file's project
// layer out of the materialized clone, long after every source has been
// resolved, and each reader takes only its own keys (ADR 26-10-16-423 §11).

// DeclaredSource is one entry of a repository's declared sources and what it
// resolved to on this machine, so a frontend can say where each came from.
// Nothing about a sandbox is silent machinery if it can be helped, and a source
// nobody asked for on the command line is exactly the kind that should be
// announced.
type DeclaredSource struct {
	// Name is the entry's key, which is both the directory looked for next to
	// the primary source and the name the source takes in the sandbox.
	Name string
	// URL is the Git URL the repository declared.
	URL string
	// Checkout is the local directory looked for beside the primary source. It
	// is reported whether or not it was there, so a message about a source
	// being cloned can say which path would have been used instead.
	Checkout string
	// Local reports whether that checkout was found, and so whether this source
	// comes from the caller's own disk rather than the URL.
	Local bool
	// Origin is the local checkout's own origin remote, reported only when it
	// disagrees with URL. A fork checked out next door is the ordinary reason
	// and is used as-is — it is what the caller has, and almost always what
	// they meant — but a directory that merely shares a name is the same
	// observation, so it is surfaced rather than resolved silently.
	Origin string
}

// ReportDeclaredSourceFunc receives each source a repository declared, as it is
// resolved. Frontends render it; this package does not print.
type ReportDeclaredSourceFunc func(DeclaredSource)

// readLocalSettings reads the settings of a source root on this machine. A
// missing file is not an error — most repositories declare nothing.
func readLocalSettings(root string) (sandboxconfig.RepositorySettings, error) {
	settings, err := sandboxconfig.ReadSettings(func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	}, sandboxconfig.LegacySourcesFile)
	if err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("%s: %w", root, err)
	}
	return settings, nil
}

// readRemoteSettings reads the settings of a remote repository at commit,
// without cloning it (ADR 26-10-10-545 §4).
//
// The sources of a create are fixed in the create request, so the file is read
// before the create and must not wait on the history. The pinned commit alone
// is fetched, with no trees or blobs, into a repository thrown away
// afterwards; reading the file then fetches the few objects on its path. A
// remote that refuses a filtered fetch gives up the commit at depth 1 instead.
// The throwaway repository is not this client's cache of the remote, which
// stays complete.
//
// ref is the branch or tag the commit was resolved from, when there was one:
// a remote that will not be asked for a commit by id is asked for that, and
// believed only while it still names the commit.
func readRemoteSettings(ctx context.Context, remote, commit, ref string) (sandboxconfig.RepositorySettings, error) {
	where := remote + "@" + commit[:min(len(commit), 12)]
	dir, err := os.MkdirTemp("", "discobox-settings-")
	if err != nil {
		return sandboxconfig.RepositorySettings{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	git := func(args ...string) (string, error) {
		return gitutil.Output(ctx, dir, nil, nil, args...)
	}
	if _, err := git("init", "--quiet", "--bare", "."); err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("read %s: %w", where, err)
	}
	// A named remote, so the objects the read needs are fetched from it on
	// demand.
	if _, err := git("remote", "add", "origin", remote); err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("read %s: %w", where, err)
	}
	_, err = git("fetch", "--quiet", "--no-tags", "--depth=1", "--filter=tree:0", "origin", commit)
	if err != nil {
		_, err = git("fetch", "--quiet", "--no-tags", "--depth=1", "origin", commit)
	}
	if err != nil && ref != "" {
		if _, refErr := git("fetch", "--quiet", "--no-tags", "--depth=1", "origin", ref); refErr == nil {
			if fetched, headErr := git("rev-parse", "--verify", "--quiet", "FETCH_HEAD"); headErr == nil && strings.TrimSpace(fetched) == commit {
				err = nil
			}
		}
	}
	if err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("read %s: %w", where, err)
	}
	// Which of the files the commit holds, in one listing, so a file it does
	// not hold is never asked for.
	listed, err := git("ls-tree", "--name-only", commit, "--",
		sandboxconfig.SettingsFile, sandboxconfig.LegacySourcesFile, sandboxconfig.LegacyProjectFile)
	if err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("read %s: %w", where, err)
	}
	held := strings.Fields(listed)
	settings, err := sandboxconfig.ReadSettings(func(path string) ([]byte, error) {
		if !slices.Contains(held, path) {
			return nil, fs.ErrNotExist
		}
		data, err := git("show", commit+":"+path)
		return []byte(data), err
	}, sandboxconfig.LegacySourcesFile)
	if err != nil {
		return sandboxconfig.RepositorySettings{}, fmt.Errorf("%s: %w", where, err)
	}
	return settings, nil
}

// declaredSources are the sources a repository's settings declare, checked.
// where names the repository in what is said about them.
//
// A malformed file is an error: it is a statement about what the sandbox must
// contain, and running without the sources it names would silently produce a
// sandbox missing them.
//
// So is a settings.json in the shape of a sources.json — nothing but $schema
// and keys it does not define, each a string: one moved into place without
// wrapping it in "sources". Only here, where sources are read: a key a newer
// Discobox added is otherwise read past, never refused.
func declaredSources(settings sandboxconfig.RepositorySettings, where string) (map[string]string, error) {
	parsed, err := settings.Settings()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", where, err)
	}
	if unknown := settings.UnknownKeys(); len(unknown) > 0 {
		var all map[string]json.RawMessage
		_ = json.Unmarshal(settings.Data, &all)
		delete(all, "$schema")
		unwrapped := len(all) == len(unknown)
		for _, value := range unknown {
			var url string
			unwrapped = unwrapped && json.Unmarshal(value, &url) == nil
		}
		if unwrapped {
			return nil, fmt.Errorf(`%s: %s holds %s at its top level, the shape of a sources.json; sources go in its "sources" object`,
				where, settings.Path, quoteKeys(unknown))
		}
	}
	return checkDeclaredSources(parsed.Sources, where+": "+settings.Path)
}

// checkDeclaredSources checks the sources a settings file declares, and
// expands each to the Git URL it names. path names the file in what is said
// about it.
func checkDeclaredSources(declared map[string]string, path string) (map[string]string, error) {
	for name, url := range declared {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("%s: a declared source has no name", path)
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("%s: declared source %q has no Git URL", path, name)
		}
		// A path is refused rather than resolved. Where to look locally is not
		// the file's to say — the checkout beside the source is found by name —
		// and a relative path here would otherwise be resolved against whatever
		// directory the caller happened to run from, quietly bringing in some
		// other repository entirely. GitHub shorthand is therefore always the
		// repository, never a directory that happens to share its name.
		source, err := sourceref.Parse(url)
		if err != nil {
			return nil, fmt.Errorf("%s: declared source %q: %w", path, name, err)
		}
		source = source.Expand()
		if source.Kind != sourceref.Git {
			return nil, fmt.Errorf("%s: declared source %q is %q, which is not a Git URL; the local checkout beside the source is found by name, so only the URL to fall back to belongs here",
				path, name, url)
		}
		if source.Path != "" {
			return nil, fmt.Errorf("%s: declared source %q is %q; //%s says where the harness starts, which only the primary source does",
				path, name, url, source.Path)
		}
		// Nor a URL that names a path on this machine. A declared source is
		// fetched by this client and delivered into the discobox, and the
		// file naming it is repository content: it must not be able to name
		// a repository on the disk of whoever happens to run the create.
		if sourceref.NormalizeRemote(source.Location) == "" {
			return nil, fmt.Errorf("%s: declared source %q is %q, which names a path on this machine and not a repository on a network",
				path, name, url)
		}
		declared[name] = source.String()
	}
	return declared, nil
}

// quoteKeys lists the keys of a repository's settings file for a person to
// read, sorted and each Go-quoted: a key is repository content, and a JSON
// escape in one decodes to a control character that would otherwise reach
// their terminal (cli/REVIEW.md).
func quoteKeys(keys map[string]json.RawMessage) string {
	quoted := make([]string, 0, len(keys))
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		quoted = append(quoted, strconv.Quote(key))
	}
	return strings.Join(quoted, ", ")
}

// declaredSourceNames is the order declared sources are resolved in. It is
// sorted rather than map order so two runs of the same repository produce the
// same sandbox, down to which of two same-named sources took the plain slug.
func declaredSourceNames(declared map[string]string) []string {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// declaredSourceCheckout is the local directory a declared source is looked for
// in: the sibling of the primary source named by the entry, so a repository
// declaring "foo" means the ../foo the caller already has checked out.
func declaredSourceCheckout(primaryRoot, name string) string {
	return filepath.Join(filepath.Dir(primaryRoot), name)
}

// resolveDeclaredSourceArg decides what a declared source resolves to: the
// local checkout beside the primary source when there is one, and the declared
// URL when there is not.
//
// The local checkout wins whatever its origin says. A fork is the usual reason
// for a mismatch and is what the caller wants used; the report carries the
// disagreement so a directory that only shares a name is visible rather than
// silently substituted.
func resolveDeclaredSourceArg(ctx context.Context, primaryRoot, name, url string) (arg string, report DeclaredSource) {
	if primaryRoot == "" {
		// A remote primary source has no directory on this machine, and so
		// nothing beside it to look in: every source it declares is its URL.
		return url, DeclaredSource{Name: name, URL: url}
	}
	checkout := declaredSourceCheckout(primaryRoot, name)
	report = DeclaredSource{Name: name, URL: url, Checkout: checkout}
	info, err := os.Stat(checkout)
	if err != nil || !info.IsDir() {
		return url, report
	}
	report.Local = true
	if origin := gitOriginURL(ctx, checkout); origin != "" && !sameGitRemote(origin, url) {
		report.Origin = origin
	}
	return checkout, report
}

// gitOriginURL is the checkout's origin remote, or empty when it has none —
// which a directory in no repository, or one that was never cloned, does not.
//
// The configured value, not `remote get-url`: get-url applies the caller's
// url.*.insteadOf rewrites, and this is reported back to the person who wrote
// the remote down. They should read what they wrote.
func gitOriginURL(ctx context.Context, dir string) string {
	out, err := gitutil.Output(ctx, dir, nil, nil, "config", "--get", "remote.origin.url")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// sameGitRemote compares two Git URLs the way a person would: the same
// repository reached over ssh, over https, with or without a .git suffix or a
// trailing slash, is the same repository. Userinfo and port are dropped for the
// same reason — they say how to reach it, not which one it is.
func sameGitRemote(a, b string) bool {
	return normalizeGitRemote(a) == normalizeGitRemote(b)
}

func normalizeGitRemote(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(value, "://"); ok {
		value = rest
	}
	// Userinfo, which is the "@" before the host rather than an @REF suffix.
	if head, rest, ok := strings.Cut(value, "@"); ok && !strings.Contains(head, "/") {
		value = rest
	}
	if host, rest, ok := strings.Cut(value, ":"); ok {
		// A port, or the scp-style separator between host and path. Either way
		// what follows is the path.
		if port, path, hasPath := strings.Cut(rest, "/"); hasPath && isAllDigits(port) {
			value = host + "/" + path
		} else {
			value = host + "/" + rest
		}
	}
	// A trailing @REF names a commit, not a repository.
	if at := strings.LastIndex(value, "@"); at > 0 {
		value = value[:at]
	}
	return strings.TrimSuffix(strings.TrimRight(value, "/"), ".git")
}

func isAllDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
