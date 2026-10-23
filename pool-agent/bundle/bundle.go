// Package bundle is the pool-agent bundle (ADR 26-10-23-551 §2): where its
// tree is placed on a pool machine and runs from, the bootstrap beside it, and
// the `install` command that makes a booted driver base a pool machine.
//
// The bundle is a FROM scratch target of pool-agent/Dockerfile holding
// FeatureDir: the agent binary, which is also the pool proxy, and units/. A
// pool machine's provider places a tree under TreeDir, the bootstrap at
// BootstrapFile, and runs that tree's agent with `install` (§1).
package bundle

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Where a pool machine keeps the bundle, beside the sandbox agent's content
// store and apart from it: a pool machine runs no sandbox agent.
const (
	// Dir holds every tree placed and the link to the one the units run.
	Dir = "/opt/discobox/pool-agent"
	// TreesDir holds each tree placed, by manifest digest (TreeDir).
	TreesDir = Dir + "/trees"
	// CurrentLink is the stable path the units run from: a link to the tree
	// install ran from last.
	CurrentLink = Dir + "/current"
	// InstalledFile records the tree the units were last restarted onto, and
	// the units they were started from: what install compares with to know
	// whether a restart is owed.
	InstalledFile = Dir + "/installed"

	// FeatureDir is the bundle's one feature, within its tree.
	FeatureDir = "features/pool-agent"
	// AgentFile is the agent within the tree.
	AgentFile = FeatureDir + "/discobox-pool-agent"
	// UnitsDir is the units within the tree.
	UnitsDir = FeatureDir + "/units"
	// AgentPath is the agent the units run.
	AgentPath = CurrentLink + "/" + AgentFile

	// BootstrapFile is the pool's poolagent.Bootstrap as JSON, root's alone,
	// which the agent's unit names to it (poolagent.EnvBootstrapFile).
	BootstrapFile = "/etc/discobox/pool-agent/bootstrap.json"

	// AgentUnit and ProxyUnit are the bundle's units: the agent, and the
	// pool proxy ordered after it.
	AgentUnit = "discobox-pool-agent.service"
	ProxyUnit = "discobox-proxy.service"

	unitDir = "/etc/systemd/system"
)

// Units are the bundle's units, in the order they start.
var Units = []string{AgentUnit, ProxyUnit}

// TreeDir is where the tree of the bundle's manifest digest is placed.
func TreeDir(digest string) string {
	return TreesDir + "/" + strings.TrimPrefix(digest, "sha256:")
}

// Config is what Install acts on. The command passes the machine itself; a
// test passes a root of its own and records the commands.
type Config struct {
	// Root is the machine's filesystem root; empty is "/".
	Root string
	// Executable is the agent running install, within the tree it installs.
	Executable string
	// Run runs a command with env added to this process's environment.
	Run func(ctx context.Context, env []string, argv ...string) error
	// LookPath finds a program on PATH, as exec.LookPath does.
	LookPath func(file string) (string, error)
	// Log is where install says what it did.
	Log io.Writer
}

// Install makes this machine a pool machine running the tree cfg.Executable
// is in, and is idempotent (ADR 26-10-23-551 §1). In order: it installs git
// where it is absent (Debian family only), before anything else moves; points
// CurrentLink at the tree; lays the units; and, unless InstalledFile says the
// units already run this tree with these units, reloads systemd, enables and
// restarts them, and records that they do. Otherwise it changes nothing, and
// only starts what is stopped. Last, it removes every tree under TreesDir but
// this one.
//
// The record is written only once the restart succeeded, so an install cut
// short after the link moved is finished by the next one rather than taken
// for done.
func Install(ctx context.Context, cfg Config) error {
	root := cfg.Root
	if root == "" {
		root = "/"
	}
	if _, err := os.Stat(filepath.Join(root, "run/systemd/system")); err != nil {
		return errors.New("systemd is not running on this machine: a pool machine runs the bundle's units")
	}
	tree, err := treeOf(root, cfg.Executable)
	if err != nil {
		return err
	}
	if err := ensureGit(ctx, root, cfg); err != nil {
		return err
	}
	if err := pointLink(filepath.Join(root, CurrentLink), tree); err != nil {
		return err
	}
	units, err := layUnits(root, filepath.Join(root, tree))
	if err != nil {
		return err
	}
	systemctl := func(args ...string) error {
		return cfg.Run(ctx, nil, append([]string{"systemctl"}, args...)...)
	}
	record := installedRecord(tree, units)
	installed := filepath.Join(root, InstalledFile)
	if current, err := os.ReadFile(installed); err == nil && bytes.Equal(current, record) {
		_, _ = fmt.Fprintf(cfg.Log, "install: %s is running; starting %s if stopped\n", tree, strings.Join(Units, ", "))
		if err := systemctl(append([]string{"start"}, Units...)...); err != nil {
			return err
		}
		return pruneTrees(root, tree)
	}
	_, _ = fmt.Fprintf(cfg.Log, "install: running %s; restarting %s\n", tree, strings.Join(Units, ", "))
	for _, args := range [][]string{
		{"daemon-reload"},
		append([]string{"enable"}, Units...),
		append([]string{"restart"}, Units...),
	} {
		if err := systemctl(args...); err != nil {
			return err
		}
	}
	if err := writeFile(installed, record); err != nil {
		return fmt.Errorf("record the installed tree: %w", err)
	}
	return pruneTrees(root, tree)
}

// installedRecord is what InstalledFile holds once the units run tree: the
// tree, and a digest of the units they were started from.
func installedRecord(tree string, units []byte) []byte {
	sum := sha256.Sum256(units)
	return []byte(tree + "\n" + hex.EncodeToString(sum[:]) + "\n")
}

// pruneTrees removes every tree under TreesDir but tree, now that nothing runs
// from them: a pool machine keeps its disk across upgrades, and each would
// otherwise stay for the life of the pool. A tree placed elsewhere leaves
// TreesDir alone.
func pruneTrees(root, tree string) error {
	if path.Dir(tree) != TreesDir {
		return nil
	}
	dir := filepath.Join(root, TreesDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == path.Base(tree) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("remove a superseded tree: %w", err)
		}
	}
	return nil
}

// treeOf is the tree, as a path on the machine, whose agent executable is.
func treeOf(root, executable string) (string, error) {
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve the running agent: %w", err)
	}
	suffix := string(filepath.Separator) + filepath.FromSlash(AgentFile)
	if !strings.HasSuffix(resolved, suffix) {
		return "", fmt.Errorf("install runs from a pool-agent bundle's tree, as <tree>/%s; this agent is %s", AgentFile, resolved)
	}
	tree := strings.TrimSuffix(resolved, suffix)
	if root != "/" {
		rel, err := filepath.Rel(root, tree)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("tree %s is outside root %s", tree, root)
		}
		tree = "/" + filepath.ToSlash(rel)
	}
	return filepath.ToSlash(tree), nil
}

// ensureGit installs git where it is absent: the one package the pool agent
// needs that the platform contract does not promise (ADR 26-10-23-551 §1),
// installed only on the Debian family (ADR 26-10-16-423 §3).
func ensureGit(ctx context.Context, root string, cfg Config) error {
	if _, err := cfg.LookPath("git"); err == nil {
		return nil
	}
	if _, err := os.Stat(filepath.Join(root, "etc/debian_version")); err != nil {
		return errors.New("git is absent and this is not a Debian-family machine, the only family install provides it on")
	}
	_, _ = fmt.Fprintln(cfg.Log, "install: installing git")
	env := []string{"DEBIAN_FRONTEND=noninteractive"}
	if err := cfg.Run(ctx, env, "apt-get", "update"); err != nil {
		return fmt.Errorf("install git: %w", err)
	}
	if err := cfg.Run(ctx, env, "apt-get", "install", "-y", "--no-install-recommends", "git"); err != nil {
		return fmt.Errorf("install git: %w", err)
	}
	return nil
}

// pointLink points link at target unless it already does, by renaming a new
// link over it so the units never find it missing.
func pointLink(link, target string) error {
	if current, err := os.Readlink(link); err == nil && current == target {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return err
	}
	tmp := link + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, link); err != nil {
		return fmt.Errorf("point %s at %s: %w", link, target, err)
	}
	return nil
}

// layUnits writes each of the tree's units where systemd reads it, unless it
// already holds the same bytes, and returns them all, in Units order.
func layUnits(root, tree string) ([]byte, error) {
	var all []byte
	for _, unit := range Units {
		content, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(UnitsDir), unit))
		if err != nil {
			return nil, fmt.Errorf("read the bundle's %s: %w", unit, err)
		}
		all = append(append(all, unit+"\n"...), content...)
		target := filepath.Join(root, unitDir, unit)
		if current, err := os.ReadFile(target); err == nil && bytes.Equal(current, content) {
			continue
		}
		if err := writeFile(target, content); err != nil {
			return nil, err
		}
	}
	return all, nil
}

// writeFile replaces path with content through a file beside it, so nothing
// reads it half written.
func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	//nolint:gosec // G306: a unit file is read by systemd and every user, as each under /etc/systemd/system is; the record names a tree and nothing secret.
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
