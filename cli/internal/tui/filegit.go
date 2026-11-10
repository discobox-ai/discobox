package tui

import (
	"context"
	"fmt"
	"path"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The files panel draws what git says about each file over the tree, the way
// an editor's explorer does: a letter at the end of a changed file's row, its
// name in that change's color, and a dot on every directory with a change
// somewhere beneath it. The letters are git's own (`git status --porcelain`):
// M modified, A added, D deleted, R renamed, C copied, ? untracked, U
// unmerged (ADR 26-11-10-203 §5).
//
// What git says is asked of the box itself (DataSource.RunCommand): nothing
// else knows it per file. The box's listing only says whether its tree is
// dirty and how large its diff is, so that is what decides when to ask again
// (filesGitTick): a box whose reported position and diffstat have not moved
// since the last read is not asked again on every poll. Opening the panel,
// giving it the keys, `r`, and every move, copy or drop it makes ask as well.

// fileGit is what git last said about one box's tree, as the panel draws it.
type fileGit struct {
	// top is the repository's top level, "" when the panel's root is in no
	// repository — which draws nothing.
	top string
	// files is each changed file's letter, and dirs the strongest change
	// beneath each directory, both by absolute path.
	files map[string]byte
	dirs  map[string]byte
	// seen is the listing's git state the last read was asked at
	// (gitSignature), and reading whether one is in flight.
	seen    string
	reading bool
}

// filesGitMsg is one read of git's status for the panel.
type filesGitMsg struct {
	box  Sandbox
	root string
	top  string
	out  string
	err  error
}

// gitSignature is what the listing says of a box's git state: when it moves,
// what git says per file may have.
func gitSignature(s Sandbox) string {
	return fmt.Sprintf("%t %s %s %t %t %d %d %d", s.Git.Known, s.Git.Branch, s.Git.Commit, s.Git.Dirty,
		s.Diff.Known, s.Diff.Files, s.Diff.Added, s.Diff.Deleted)
}

// readGit asks the box on screen what git says about its tree: where the
// repository's top level is, then its status. Paths in the status are from
// that top level, not from the panel's root.
func (m *Model) readGit() tea.Cmd {
	t := &m.fileTree
	if t.root == "" || t.git.reading || !m.filesShown() {
		return nil
	}
	ds := m.ds.ForServer(m.paneBox.Server)
	if ds == nil {
		return nil
	}
	t.git.reading = true
	t.git.seen = gitSignature(m.currentBox())
	box, root, parent := m.paneBox, t.root, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, filesTimeout)
		defer cancel()
		top, err := ds.RunCommand(ctx, box.ID, root, []string{"git", "rev-parse", "--show-toplevel"})
		if err != nil {
			return filesGitMsg{box: box, root: root, err: err}
		}
		// Not a repository is an answer, not a failure: there is nothing to
		// draw.
		if top.ExitCode != 0 {
			return filesGitMsg{box: box, root: root}
		}
		dir := strings.TrimSpace(top.Stdout)
		status, err := ds.RunCommand(ctx, box.ID, dir, []string{"git", "status", "--porcelain=v1", "-z", "--untracked-files=all"})
		if err != nil {
			return filesGitMsg{box: box, root: root, err: err}
		}
		if status.ExitCode != 0 {
			return filesGitMsg{box: box, root: root, err: fmt.Errorf("git status: %s", strings.TrimSpace(status.Stderr))}
		}
		return filesGitMsg{box: box, root: root, top: dir, out: status.Stdout}
	}
}

// filesGitRead takes one read of git's status. A read that failed keeps what
// was drawn: the overlay is a hint, and a blink to nothing on one failed
// read would say every change had gone.
func (m *Model) filesGitRead(msg filesGitMsg) tea.Cmd {
	t := m.fileTreeOf(msg.box)
	if t == nil {
		return nil
	}
	t.git.reading = false
	if msg.err != nil || msg.root != t.root {
		return nil
	}
	t.git.top = msg.top
	t.git.files, t.git.dirs = parsePorcelain(msg.top, msg.out)
	return nil
}

// filesGitTick is the workspace's poll looking at the listing: the box's git
// state moved since the last read, so git is asked again.
func (m *Model) filesGitTick() tea.Cmd {
	t := &m.fileTree
	if !m.filesShown() || t.root == "" || t.git.reading {
		return nil
	}
	if gitSignature(m.currentBox()) == t.git.seen {
		return nil
	}
	return m.readGit()
}

// parsePorcelain reads `git status --porcelain=v1 -z` from the repository
// at top: each changed file's letter, and the strongest change beneath each
// directory, by absolute path. A rename's entry is followed by the path it
// was renamed from, which is not drawn: it is gone from the tree.
func parsePorcelain(top, out string) (files, dirs map[string]byte) {
	files, dirs = map[string]byte{}, map[string]byte{}
	if top == "" {
		return files, dirs
	}
	fields := strings.Split(out, "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		x, y, name := entry[0], entry[1], entry[3:]
		if x == 'R' || x == 'C' {
			i++ // the path it came from
		}
		letter := gitLetter(x, y)
		if letter == 0 {
			continue
		}
		full := path.Join(top, name)
		files[full] = letter
		for dir := path.Dir(full); ; dir = path.Dir(dir) {
			if gitRank(letter) > gitRank(dirs[dir]) {
				dirs[dir] = letter
			}
			if dir == top || dir == "/" || dir == "." {
				break
			}
		}
	}
	return files, dirs
}

// gitLetter is the one letter a porcelain entry is drawn as: unmerged first,
// then untracked, then the working tree's change over the index's, as the
// working tree is what is on disk.
func gitLetter(x, y byte) byte {
	switch {
	case x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D'):
		return 'U'
	case x == '?' && y == '?':
		return '?'
	case x == '!':
		return 0
	case y != ' ':
		return y
	case x != ' ':
		return x
	}
	return 0
}

// gitRank orders changes for a directory's dot: the one that most needs
// looking at wins.
func gitRank(letter byte) int {
	switch letter {
	case 'U':
		return 5
	case 'M', 'R', 'C', 'T':
		return 4
	case 'D':
		return 3
	case 'A', '?':
		return 2
	case 0:
		return 0
	}
	return 1
}

// gitStyle is the color a change is drawn in: additions and deletions as the
// diffs draw them, a change to what was there as a warning, a conflict as an
// error (assets/brand/LANGUAGE.md).
func (m *Model) gitStyle(letter byte) lipgloss.Style {
	switch letter {
	case 'U':
		return m.st.statusER
	case 'A', '?':
		return m.st.add
	case 'D':
		return m.st.del
	}
	return m.st.statusWA
}

// gitMark is what a row carries at its end for git: a changed file's letter,
// a dot on a directory with a change beneath it, or nothing.
func (m *Model) gitMark(e FileEntry) (mark string, letter byte) {
	g := &m.fileTree.git
	if e.Dir {
		if l := g.dirs[e.Path]; l != 0 {
			return m.gitStyle(l).Render("•"), l
		}
		return "", 0
	}
	if l := g.files[e.Path]; l != 0 {
		return m.gitStyle(l).Render(string(l)), l
	}
	return "", 0
}
