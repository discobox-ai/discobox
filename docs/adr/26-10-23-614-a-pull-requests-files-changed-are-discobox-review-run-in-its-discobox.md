# 26-10-23-614 — A pull request's files changed are discobox-review run in its discobox

- **Status**: Accepted
- **Date**: 2026-10-23

## Context

The console's pull request tab shows the files changed behind `f`. It embedded
discobox-review's window (`github.com/discobox-ai/review/ui`) over a source the
CLI read from GitHub: the three-dot compare between the two commits the tab
read, each commit's diff, and each file at either, one request a file. That
source kept no review, so the window was a reader: no card, no approvals, no
comments, and nothing an agent had left about the change.

A discobox delivering a pull request reviews it with discobox-review before it
pushes, and that review — every reviewer's approvals and comments, and the
review card the person reviewing next opens on — is kept in the discobox's git
directory, under the branch the pull request is from. The discobox has both of
the pull request's commits: it made the head, and it fetched the base.

## Decision

1. **`f` runs the image's discobox-review in the discobox**, as
   `discobox-review -by git-user -base <BaseSHA> -head <HeadSHA>` in its
   primary source (`DataSource.PullReview`). `-head` is discobox-review's: the
   change from the merge base up to that commit, rather than up to the working
   tree, under the review the git directory keeps for the branch checked out.
   It is the binary the diff tool runs, whatever a repository declares in the
   diff tool's place: the tab wants these flags understood.
   The review shown is the one the discobox keeps for the branch it has
   checked out, so that has to be the pull request's, and the head has to be
   there — one pushed from somewhere else is not: both are looked for first,
   and what is missing is said in the tab rather than started over. The
   base need not be there: GitHub's is its base branch's tip as it is now, so
   the base branch goes along (`-fallback-base origin/<base>`), from which the
   merge base is the same for a head the discobox made. A base discobox-review
   could not find is never written into the review.
2. **It is drawn in the tab, from a terminal of its own**, under the tab's head.
   Every key but the leader is the window's, so quitting it is the way back to
   the conversation. The session is labeled (`Exec.FilesChanged`) so the
   workspace's listing never opens it as a tab, and it ends with the window,
   the tab, the workspace and the console, which waits for those ends on its
   way out.
3. **Nothing of the files changed is read from GitHub.** The CLI no longer
   imports discobox-review, and `github.PullSource` and its `safeSource` go.

## Alternatives considered

- **Keep the embedded window, over a local checkout.** The person's machine
  may have neither commit, and does not have the review: the card and
  approvals are in the discobox's git directory. Copying the review file out
  of the box each time the tab opened would be a second way to reach state the
  discobox already serves.
- **Keep the embedded window, over GitHub, and copy the review file in.** Two
  sources of one change, and the copy's line numbers are of the discobox's
  tree, which GitHub's diff only matches while nobody has pushed.
- **Open it as the diff tool, in a tab of its own.** Tools are one session
  per id; the diff tool's is the working tree's, and a second with other
  arguments would be reopened by the listing as that one after a restart.

## Consequences

- The files changed need the discobox up, with the pull request's branch
  checked out and its head in it, and a sandbox-agent image whose
  discobox-review has `-head` and `-fallback-base`: an older one exits at
  once, and the tab holds its last screen, saying so, until a key.
- A comment or approval made in the files changed lands in the discobox's
  review, where the agent that made the change reads it.
- `f` and `r` on the files changed are discobox-review's own keys now; the
  tab's are on the conversation.
- A console older than this one lists a files changed session as a shell,
  and opens it as a tab while it runs.
- What draws a pull request's diff is a terminal emulator over the box's own
  discobox-review, which draws control characters as pictures as it does in
  the diff tool; the CLI no longer escapes the diff on the way.

## Deferred

- **A session a console did not end.** One left by a console that crashed,
  or whose end the discobox refused, keeps running unlisted until the
  discobox restarts. Sweeping them from the listing needs to tell a dead
  console's session from a live one's on another machine showing the same
  box, which nothing records. Revisit when such sessions are seen piling up,
  or when an exec can be told to end once nothing is attached.
