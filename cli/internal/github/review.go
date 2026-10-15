package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/discobox-ai/review/source"
)

// diffMedia is the media type GitHub sends a comparison or a commit in as a
// unified diff, the one `git apply` takes.
const diffMedia = "application/vnd.github.diff"

// rawMedia is a file's own bytes, rather than JSON about it.
const rawMedia = "application/vnd.github.raw+json"

// comparePages bounds how many of a comparison's commits are read, 100 a
// page. GitHub lists 250 at most.
const comparePages = 3

// filePages bounds how much of a pull request's list of files is read when its
// diff has to be put together from them: 100 a page, and GitHub stops listing
// them at 3,000.
const filePages = 30

// PullSource is a pull request as discobox-review's window reads it: the
// change GitHub's Files changed shows — from where the head left its base to
// the head — the commits it was made in, and the text of any file at any of
// them.
//
// It is measured between the two commits it was made with, not the branches:
// the console reads the pull request and its review from the same head, and a
// push asks for a new source rather than moving this one. It has no working
// tree and keeps no review, so it is neither a source.Worktree nor a
// source.Store, and the window offers no checkout, editor, comment or
// approval over it.
type PullSource struct {
	c   *Client
	ctx context.Context
	// repo is the API path of the repository, and base and head the commits
	// the pull request was read at; baseRef and headRef are the branches.
	repo             string
	number           int
	base, head       string
	baseRef, headRef string

	mu sync.Mutex
	// files are the files read so far, by commit and path. A commit's file
	// never changes, so nothing read is read twice.
	files map[fileAt]string
	// commits are the commits' own diffs, for the same reason.
	commits map[string]string
}

type fileAt struct{ commit, path string }

// PullSource is the source for pull request number of owner/name, read at
// base and head (Pull.BaseSHA, Pull.HeadSHA); baseRef and headRef are its
// branches, as the window names them. ctx bounds every read it makes.
func (c *Client) PullSource(ctx context.Context, owner, name string, number int, base, head, baseRef, headRef string) *PullSource {
	return &PullSource{
		c: c, ctx: ctx, repo: repoPath(owner, name), number: number,
		base: base, head: head, baseRef: baseRef, headRef: headRef,
		files: map[fileAt]string{}, commits: map[string]string{},
	}
}

var _ source.Source = (*PullSource)(nil)

type rawCompare struct {
	MergeBase struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	AheadBy  int         `json:"ahead_by"`
	BehindBy int         `json:"behind_by"`
	Commits  []rawCommit `json:"commits"`
}

type rawCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string     `json:"message"`
		Author  rawGitUser `json:"author"`
	} `json:"commit"`
	Author *rawUser `json:"author"`
}

// Scan reads the comparison of base and head, which is what GitHub's Files
// changed is: three dots, so from their merge base. base and context are the
// window's and are not GitHub's to take — a pull request is measured from
// where it is measured from, and GitHub sends three lines of context, which
// the window fills out from the files themselves.
func (s *PullSource) Scan(_ string, _ int) (source.Scan, error) {
	path := s.repo + "/compare/" + s.base + "..." + s.head
	var cmp rawCompare
	next := path + "?per_page=100"
	for page := 0; next != "" && page < comparePages; page++ {
		var raw rawCompare
		link, err := s.c.get(s.ctx, next, &raw)
		if err != nil {
			return source.Scan{}, err
		}
		if page == 0 {
			cmp = raw
		} else {
			cmp.Commits = append(cmp.Commits, raw.Commits...)
		}
		next = nextPage(link)
	}

	scan := source.Scan{
		Branch:  s.headRef,
		HasHEAD: true,
		Base: source.Base{
			Ref: s.baseRef, SHA: cmp.MergeBase.SHA, Commit: shortCommit(cmp.MergeBase.SHA),
			Ahead: cmp.AheadBy, Behind: cmp.BehindBy,
		},
	}
	// GitHub lists a comparison's commits oldest first; the window wants the
	// newest on top, as git log has them.
	for _, raw := range slices.Backward(cmp.Commits) {
		author := raw.Commit.Author.Name
		if raw.Author != nil && raw.Author.Login != "" {
			author = raw.Author.Login
		}
		scan.Commits = append(scan.Commits, source.Commit{
			Hash: raw.SHA, Short: shortCommit(raw.SHA), Subject: firstLine(raw.Commit.Message),
			Author: author, When: raw.Commit.Author.Date.Format("Jan 2 2006"),
		})
	}

	diff, err := s.c.text(s.ctx, path, diffMedia)
	if err != nil {
		// Too big a diff for GitHub to send whole is refused rather than cut,
		// and the files' own patches are what is left: the same lines, as far
		// as GitHub will list the files and send each one's.
		var refused *Error
		if !errors.As(err, &refused) || (refused.Status != http.StatusNotAcceptable && refused.Status != http.StatusUnprocessableEntity) {
			return source.Scan{}, err
		}
		diff, scan.Incomplete, err = s.patches()
		if err != nil {
			return source.Scan{}, err
		}
	}
	scan.Diff = diff
	if len(cmp.Commits) < cmp.AheadBy {
		scan.Incomplete = strings.TrimPrefix(scan.Incomplete+fmt.Sprintf("; %d of %d commits listed", len(cmp.Commits), cmp.AheadBy), "; ")
	}
	return scan, nil
}

// patches is the pull request's diff put back together from each file's own
// patch, and what it says about the diff it is not. A comparison lists 300
// files at most; the pull request's own list goes to 3,000, which is as far as
// GitHub lists one. It is the list as GitHub has the pull request now, which is
// the head this source was opened over unless a push has landed since — and a
// push is a new window.
func (s *PullSource) patches() (diff, incomplete string, err error) {
	var files []rawFile
	next := fmt.Sprintf("%s/pulls/%d/files?per_page=100", s.repo, s.number)
	for page := 0; next != "" && page < filePages; page++ {
		var items []rawFile
		link, err := s.c.get(s.ctx, next, &items)
		if err != nil {
			return "", "", err
		}
		files = append(files, items...)
		next = nextPage(link)
	}
	diff, missing := patchDiff(files)
	incomplete = "GitHub would not send the whole diff: this is each file's own patch"
	if len(missing) > 0 {
		incomplete += fmt.Sprintf("; %d of %d files have none", len(missing), len(files))
	}
	if next != "" {
		incomplete += fmt.Sprintf("; only the first %d files are listed", len(files))
	}
	return diff, incomplete, nil
}

// CommitDiff is one commit's own diff, as GitHub sends it, with its three
// lines of context whatever context asks for.
func (s *PullSource) CommitDiff(hash string, _ int) (string, error) {
	s.mu.Lock()
	diff, ok := s.commits[hash]
	s.mu.Unlock()
	if ok {
		return diff, nil
	}
	diff, err := s.c.text(s.ctx, s.repo+"/commits/"+url.PathEscape(hash), diffMedia)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.commits[hash] = diff
	s.mu.Unlock()
	return diff, nil
}

// FileContent is path's text at commit, or at the head when commit is empty.
//
// The window reads every changed file as each scan lands, to show what the
// diff leaves out around its hunks, and that is one request a file. With no
// token there are sixty an hour, which one large pull request would spend: so
// with none no file is read, and the diff is shown without them.
//
// A file read is kept, since a commit's file never changes; a failure is not,
// so a timeout or a refusal is asked again the next time.
func (s *PullSource) FileContent(commit, path string) (string, error) {
	if s.c.bearer(s.ctx) == "" {
		return "", ErrNoToken
	}
	if commit == "" {
		commit = s.head
	}
	key := fileAt{commit, path}
	s.mu.Lock()
	text, ok := s.files[key]
	s.mu.Unlock()
	if ok {
		return text, nil
	}
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	text, err := s.c.text(s.ctx, s.repo+"/contents/"+strings.Join(segments, "/")+"?ref="+url.QueryEscape(commit), rawMedia)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.files[key] = text
	s.mu.Unlock()
	return text, nil
}

// patchDiff is a unified diff put back together from GitHub's per-file
// patches, with the headers git would have written over each, and the files
// that had none to give (a binary, or one too large).
func patchDiff(files []rawFile) (diff string, missing []string) {
	var out strings.Builder
	for _, file := range files {
		if file.Patch == "" {
			missing = append(missing, file.Filename)
			continue
		}
		from, to := "a/"+file.Filename, "b/"+file.Filename
		if file.PreviousFilename != "" {
			from = "a/" + file.PreviousFilename
		}
		fmt.Fprintf(&out, "diff --git %s %s\n", from, to)
		switch file.Status {
		case "added":
			fmt.Fprintf(&out, "new file mode 100644\n--- /dev/null\n+++ %s\n", to)
		case "removed":
			fmt.Fprintf(&out, "deleted file mode 100644\n--- %s\n+++ /dev/null\n", from)
		default:
			fmt.Fprintf(&out, "--- %s\n+++ %s\n", from, to)
		}
		out.WriteString(file.Patch)
		if !strings.HasSuffix(file.Patch, "\n") {
			out.WriteString("\n")
		}
	}
	return out.String(), missing
}
