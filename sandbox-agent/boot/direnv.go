package boot

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// seedDirenvConfig whitelists the sandbox's own source trees in the run user's
// direnv config, so a repository that ships an .envrc loads it in every shell
// the sandbox starts without somebody first typing `direnv allow`.
//
// The whitelist is the only form of this that can be seeded at all. direnv's
// `allow` records a file named by a hash of the .envrc's absolute path *and* its
// contents: there is nothing to hash before the source is wired, and the first
// edit to the file revokes what was recorded -- so an allow cannot be shipped in
// the image, in /etc/skel, or written here. A whitelisted prefix is a property
// of the directory, so it survives both, and it covers an .envrc in a
// subdirectory of the tree as well as the one at its root.
//
// The targets come from the manifest rather than a constant because that is
// where they are decided: the primary source defaults to /workspace, but any
// source may name its own destination directory.
//
// Trusting these directories is not a new decision. A sandbox exists to run the
// code it was given, and its harness, services, and hooks already do.
func (b *booter) seedDirenvConfig(id identity, sources []sandboxconfig.Source) error {
	prefixes := sourcePrefixes(sources)
	if len(prefixes) == 0 {
		return nil
	}
	dir := filepath.Join(id.home, ".config", "direnv")
	path := filepath.Join(dir, "direnv.toml")
	// Written only when absent. direnv reads exactly one config file, so a
	// person adding a prefix of their own has nowhere else to put it, and a
	// rewrite on the next start would take it back. This is the file-level form
	// of the rule seedGitConfig applies per key.
	switch _, err := os.Stat(path); {
	case err == nil:
		return nil
	case !os.IsNotExist(err):
		return err
	}
	// Both components are created here rather than by MkdirAll alone because
	// each has to end up owned by the run user: ~/.config is as likely to be
	// missing as the direnv directory under it, and boot writes as root.
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := os.Chown(d, id.uid, id.gid); err != nil {
			return err
		}
	}
	// 0600: nothing but the run user's own direnv reads this, and it is the
	// file that decides which directories get to execute code.
	if err := os.WriteFile(path, []byte(direnvConfig(prefixes)), 0o600); err != nil {
		return err
	}
	return os.Chown(path, id.uid, id.gid)
}

// sourcePrefixes is the whitelist itself: every wired source target, cleaned,
// in manifest order and without repeats. A source that names no target is
// skipped rather than contributing an empty prefix -- "" is a prefix of every
// path, so it would whitelist the whole filesystem.
func sourcePrefixes(sources []sandboxconfig.Source) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, s := range sources {
		target := strings.TrimSpace(s.Target)
		if target == "" {
			continue
		}
		target = filepath.Clean(target)
		if target == "/" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}

// direnvConfig renders the whitelist as direnv.toml. Paths are quoted rather
// than interpolated: a TOML basic string and a Go quoted string agree on the
// escapes a filesystem path can need.
func direnvConfig(prefixes []string) string {
	var b strings.Builder
	b.WriteString("# Written by the Discobox sandbox boot flow, once, when this file did not\n")
	b.WriteString("# exist. The sandbox's own source trees are whitelisted so their .envrc\n")
	b.WriteString("# loads without an explicit `direnv allow`. Edit freely; boot will not\n")
	b.WriteString("# rewrite it.\n")
	b.WriteString("[whitelist]\nprefix = [\n")
	for _, p := range prefixes {
		fmt.Fprintf(&b, "  %s,\n", strconv.Quote(p))
	}
	b.WriteString("]\n")
	return b.String()
}
