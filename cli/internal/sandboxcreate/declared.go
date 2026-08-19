package sandboxcreate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/obot-platform/discobox/internal/gitutil"
)

// declaredSourcesFile is where a repository names the other repositories it is
// worked on with: a JSON object of name to Git URL, optionally with an @REF
// suffix, at the source's root.
//
//	{"foo": "https://github.com/acme/foo", "bar": "git@github.com:acme/bar@main"}
//
// It is read here, on the client, and not by the pool or the server: deciding
// what a declared source resolves to means looking at this machine's disk for a
// checkout of it, which nothing else in the system can see. That is also why it
// is a file of its own rather than a field of .discobox/project.json — that one
// is read by pool-agent, out of the materialized clone, long after every source
// has been resolved (ADR 0012 §7).
const declaredSourcesFile = ".discobox/sources.json"

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

// readDeclaredSources reads the sources a source root declares. A missing file
// is not an error — most repositories declare nothing — but a malformed one is:
// it is a statement about what the sandbox must contain, and running without
// the sources it names would silently produce a sandbox missing them.
func readDeclaredSources(root string) (map[string]string, error) {
	path := filepath.Join(root, filepath.FromSlash(declaredSourcesFile))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var declared map[string]string
	if err := json.Unmarshal(data, &declared); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
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
		// other repository entirely.
		if source, _, _ := splitRunSourceRef(url); !isRemoteGitSource(source) {
			return nil, fmt.Errorf("%s: declared source %q is %q, which is not a Git URL; the local checkout beside the source is found by name, so only the URL to fall back to belongs here",
				path, name, url)
		}
	}
	return declared, nil
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
func gitOriginURL(ctx context.Context, dir string) string {
	out, err := gitutil.Output(ctx, dir, nil, nil, "remote", "get-url", "origin")
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
