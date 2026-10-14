package tui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/discobox-ai/discobox/termpane"
)

// The issue tab is the GitHub issue a discobox is tagged with (`issue=N`), read
// in the workspace rather than in a browser: the title, its state, who opened
// it and when, its assignees, labels, type, milestone and sub-issues, the
// description, and the whole timeline — every comment and the events between
// them — with a field at the foot to answer it. A plain click on the header's
// `issue #N` opens it, Ctrl-click still follows the link to the page, and the
// leader's i opens it from the keyboard.
//
// The pull request it is tagged with (`pr=N`) is the same tab, behind `PR #N`
// and the leader's p: to GitHub a pull request is an issue, so everything here
// is its too, and pull.go draws the rest of its page — the branches, the
// reviewers, the checks, the reviews with their threads on the diff, and the
// files changed as a second page of the tab.
//
// It is a tab on the right, among the shells: a pane like any other in the
// strip, with its number, its place in ←/→, the column's [+] and [x], and the
// whole key map behind the leader. Its termpane is never attached to a
// stream — it is there for the key map, which works with nothing attached —
// and the box draws the issue in place of a terminal's grid.
//
// GitHub is read from this machine, as the person at it (the data source's
// token is the one `gh` is logged in with):
// the issue is theirs to read and the comment is theirs to post, and neither
// goes through the discobox or the server.

const (
	// issueKey opens the tab behind the leader, or focuses it when it is open.
	// I for issue: free in the key map the list and the workspace share.
	issueKey = "i"
	// pullKey does the same for the pull request. P for pull request, and
	// free in the same key map.
	pullKey = "p"
	// issuePostKey posts the comment being written. Enter is a newline in a
	// comment — a comment is paragraphs — so posting is a chord nobody types
	// by accident: what it does is public.
	issuePostKey = "ctrl+s"
	// issueRefreshEvery is how often an open issue is read again, so a comment
	// posted from the page shows up here without asking.
	issueRefreshEvery = time.Minute
	// issueTimeout bounds one read or one post.
	issueTimeout = 30 * time.Second
	// issueComposerRows is how tall the comment field is.
	issueComposerRows = 6
	// issueLabelWidth is the label column of the facts under the title.
	issueLabelWidth = 11
	// issueRenderers is how many markdown renderers a tab keeps: the page's
	// width and a review thread's, with room for a resize between frames.
	issueRenderers = 4
)

// issuePane is what a pane drawing an issue holds instead of a terminal.
type issuePane struct {
	// repository is the web address the issue is numbered in
	// (Sandbox.Repository), and url the issue's own page.
	repository string
	number     int
	url        string
	// pull is whether it is a pull request: what the tag it was opened from
	// said, until a read says what the number is.
	pull bool

	// issue is the last read, nil until the first one lands. err is why the
	// last read failed; an issue already on screen stays there under it.
	issue   *Issue
	err     string
	loading bool
	// reads counts the reads asked for. Only the latest one is taken: one
	// already in flight when a post or a close asked for another may land
	// after it, and would put back what was there before.
	reads int
	// toEnd scrolls to the foot once the next read lands: a comment just
	// posted is the thing to look at.
	toEnd bool

	// conversation is the page every issue has, and changes a pull request's
	// files changed; files is which of the two is on screen. Each keeps its
	// own place.
	conversation issuePage
	changes      issuePage
	files        bool
	// The files changed are a list, and diff is the diff of the one under its
	// cursor once it is opened (fileOpen). fileRow is the line of changes the
	// first file is on, and fileTop the row of the tab it was last drawn on,
	// which is where the pointer finds the files.
	fileCursor int
	fileOpen   bool
	diff       issuePage
	fileRow    int
	fileTop    int
	// pushed is the head the person was last shown, once a read of the tab's
	// own brought a newer one: the tab says so until they ask for a read (r).
	// asked is that read under way: the head it brings is one they asked to
	// see, not one that moved under them.
	pushed string
	asked  bool
	// mergeRow is the line of the conversation the ready band is on, or -1,
	// and mergeAt the row of the tab it was last drawn on (as fileTop is).
	mergeRow int
	mergeAt  int
	// md are the markdown renderers by the width they wrap to: a comment in
	// a review thread is drawn narrower than one in the timeline.
	md map[int]*glamour.TermRenderer

	// page is how many lines the last draw had room for.
	page int

	// composing is whether the keys go to the comment field. Its text stays
	// while the tab is open, written or not, so stepping out to look at a
	// terminal does not lose a comment half written.
	composing bool
	composer  textarea.Model
	posting   bool
	// changing is a close, a reopen or a merge on its way to GitHub.
	changing bool
	// settles counts the reads asked for early because GitHub had yet to say
	// whether a pull request can merge (pullSettle).
	settles int
}

// issuePage is one page of the tab drawn at a width — kept until a read or a
// resize makes it wrong, since rendering markdown on every frame is a cost the
// frame rate pays — and the first line of it on screen.
type issuePage struct {
	lines  []string
	width  int
	offset int
}

// shown is the page on screen.
func (v *issuePane) shown() *issuePage {
	switch {
	case v.files && v.fileOpen:
		return &v.diff
	case v.files:
		return &v.changes
	}
	return &v.conversation
}

// label is what the tab is called, and what the header calls it.
func (v *issuePane) label() string {
	if v.pull {
		return "PR #" + itoa(v.number)
	}
	return "issue #" + itoa(v.number)
}

// noun is what it is, in a sentence.
func (v *issuePane) noun() string {
	if v.pull {
		return "pull request"
	}
	return "issue"
}

// address is its page on GitHub.
func (v *issuePane) address() string {
	if v.pull {
		return v.repository + "/pull/" + itoa(v.number)
	}
	return v.repository + "/issues/" + itoa(v.number)
}

// openIssueMsg is the leader plus issueKey inside a pane, or plus pullKey for
// the pull request.
type openIssueMsg struct{ pull bool }

// The issue's own messages are addressed to the pane by its id, like a pane's
// own commands: a read still in flight for a tab that was closed finds no pane
// and is dropped.

// issueLoadedMsg is one read of the issue.
type issueLoadedMsg struct {
	id    int
	read  int
	issue Issue
	err   error
}

// issueTickMsg is time to read the issue again.
type issueTickMsg struct{ id int }

// issuePostedMsg is a comment posted, or why it was not.
type issuePostedMsg struct {
	id  int
	err error
}

// issueStateMsg is a close or a reopen done, or why it was not.
type issueStateMsg struct {
	id     int
	state  string
	reason string
	err    error
}

// issueEditedMsg is the comment back from $EDITOR.
type issueEditedMsg struct {
	id   int
	path string
	err  error
}

// workNumber is the issue (`issue=`) or the pull request (`pr=`) the discobox
// is tagged with — the first, when it is tagged with several — for a discobox
// whose repository is on GitHub.
func (s Sandbox) workNumber(pull bool) (int, bool) {
	prefix := "issue="
	if pull {
		prefix = "pr="
	}
	for _, tag := range s.Tags {
		if !strings.HasPrefix(tag, prefix) || s.tagURL(tag) == "" {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(tag, prefix))
		if err == nil {
			return n, true
		}
	}
	return 0, false
}

// issueTab is the open tab on an issue, if there is one.
func (m *Model) issueTab(repository string, number int) *pane {
	for _, p := range m.shells.all() {
		if p.issue != nil && p.issue.repository == repository && p.issue.number == number {
			return p
		}
	}
	return nil
}

// issueOf is the pane a message is for, and its issue, while it is still open
// on one.
func (m *Model) issueOf(id int) (*pane, *issuePane) {
	p := m.paneByID(id)
	if p == nil || p.issue == nil {
		return nil, nil
	}
	return p, p.issue
}

// openIssue opens a tab on one issue or pull request of the discobox on screen
// and focuses it. The same one already open is only focused: reading it again
// would throw away the place it was read to.
func (m *Model) openIssue(number int, pull bool) tea.Cmd {
	box := m.currentBox()
	v := &issuePane{repository: box.Repository, number: number, pull: pull, composer: m.newIssueComposer()}
	v.url = v.address()
	if box.Repository == "" {
		return status("%s is not on GitHub — this discobox has no GitHub repository", v.label())
	}
	if p := m.issueTab(box.Repository, number); p != nil {
		m.focusPane(p)
		return nil
	}
	m.nextPaneID++
	_, width := m.columns(m.shells.len() + 1)
	cols, rows := m.paneCells(width)
	term := termpane.New(m.paneOptions(paneWorkspace, true)...)
	term.SetSize(cols, rows)
	p := &pane{
		id: m.nextPaneID, term: term, sandbox: box,
		title: v.label(),
		issue: v,
	}
	at := m.shells.insert(p, Exec{CreatedAt: time.Now()}, m.onShells)
	m.onShells, m.shells.active = true, at
	m.layout()
	return tea.Batch(m.loadIssue(p, true), issueTick(p.id))
}

// toggleIssue is the leader's i, or its p for the pull request: the tab on the
// discobox's issue, opened or focused.
func (m *Model) toggleIssue(pull bool) tea.Cmd {
	number, ok := m.currentBox().workNumber(pull)
	switch {
	case ok:
		return m.openIssue(number, pull)
	case pull:
		return status("no pull request to show — tag this discobox pr=N, on a GitHub repository")
	}
	return status("no issue to show — tag this discobox issue=N, on a GitHub repository")
}

// newIssueComposer is the comment field: the prompt's look without its
// chevron, and a cursor that holds still — nothing routes a blink to it.
func (m *Model) newIssueComposer() textarea.Model {
	ta := textarea.New()
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.Prompt = ""
	ta.Placeholder = "Leave a comment — Markdown"
	styles := textarea.Styles{}
	if m.st.color {
		styles = textarea.DefaultDarkStyles()
	}
	styles.Focused.CursorLine = lipgloss.NewStyle()
	styles.Cursor.Blink = false
	ta.SetStyles(styles)
	return ta
}

// loadIssue reads a tab's issue again: in full when someone asked to see it
// now, and otherwise as cheaply as the data source can say it has not changed.
func (m *Model) loadIssue(p *pane, full bool) tea.Cmd {
	v := p.issue
	v.loading = true
	v.reads++
	ctx, ds, id, read, repository, number := m.ctx, m.ds, p.id, v.reads, v.repository, v.number
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, issueTimeout)
		defer cancel()
		issue, err := ds.Issue(ctx, repository, number, full)
		return issueLoadedMsg{id: id, read: read, issue: issue, err: err}
	}
}

func issueTick(id int) tea.Cmd {
	return tick(issueRefreshEvery, func(time.Time) tea.Msg { return issueTickMsg{id: id} })
}

// issueLoaded takes a read. A failure leaves what was read before on screen
// and says why it is not newer. What was read says what the number is, which
// the tag it was opened from may have had wrong: `issue=9` on a pull request
// is a pull request.
func (m *Model) issueLoaded(msg issueLoadedMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil || msg.read != v.reads {
		return nil
	}
	v.loading = false
	asked := v.asked
	v.asked = false
	if msg.err != nil {
		v.err = msg.err.Error()
		return nil
	}
	if asked {
		v.pushed = ""
	}
	issue := msg.issue
	was := v.issue
	v.issue, v.err = &issue, ""
	v.conversation.lines, v.changes.lines, v.diff.lines = nil, nil, nil
	v.pull = issue.PullRequest
	v.url, p.title = v.address(), v.label()
	if issue.Pull == nil {
		v.files = false
		return nil
	}
	if was != nil && was.Pull != nil && was.Pull.HeadSHA != issue.Pull.HeadSHA {
		// A push: what was read here is no longer what would be merged, and
		// the files are another list. The cursor follows its file by name, and
		// a diff that was open is left only if its file is still there.
		if v.pushed == "" && !asked {
			v.pushed = was.Pull.HeadSHA
		}
		at := -1
		if v.fileCursor < len(was.Pull.Files) {
			path := was.Pull.Files[v.fileCursor].Path
			at = slices.IndexFunc(issue.Pull.Files, func(file PullFile) bool { return file.Path == path })
		}
		if at < 0 {
			v.fileOpen = false
		} else {
			v.fileCursor = at
		}
		if v.fileOpen {
			v.diff.offset = 0
		}
	}
	// The files may be fewer than they were: a push took one out.
	v.fileCursor = min(v.fileCursor, max(len(issue.Pull.Files)-1, 0))
	if len(issue.Pull.Files) == 0 {
		v.fileOpen = false
	}
	if pullReady(issue) {
		// On the tab itself, so it is seen from whichever pane has the keys.
		p.title += " ●"
	}
	return m.pullSettle(p)
}

// issueTicked reads the issue again on the tab's own clock, unless a read or a
// post is already under way.
func (m *Model) issueTicked(msg issueTickMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil {
		return nil
	}
	if v.loading || v.posting {
		return issueTick(p.id)
	}
	return tea.Batch(m.loadIssue(p, false), issueTick(p.id))
}

// postComment sends what the field holds.
func (m *Model) postComment(p *pane) tea.Cmd {
	v := p.issue
	if v.posting {
		return nil
	}
	// Posted as written: an indented first line is a code block in markdown,
	// and trimming it would change what the comment says.
	body := v.composer.Value()
	if strings.TrimSpace(body) == "" {
		return status("nothing to post — the comment is empty")
	}
	v.posting = true
	ctx, ds, id, repository, number := m.ctx, m.ds, p.id, v.repository, v.number
	post := func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, issueTimeout)
		defer cancel()
		return issuePostedMsg{id: id, err: ds.CommentOnIssue(ctx, repository, number, body)}
	}
	return tea.Batch(post, status("posting the comment on #%d…", number))
}

// issuePosted clears the field once the comment is GitHub's, and reads the
// issue again to show it where it landed. A refusal keeps every word of it.
func (m *Model) issuePosted(msg issuePostedMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil {
		return nil
	}
	v.posting = false
	if msg.err != nil {
		return m.report(true, "the comment was not posted: %v", msg.err)
	}
	v.composer.Reset()
	v.composer.Blur()
	v.composing = false
	v.toEnd, v.files = true, false
	return tea.Batch(m.loadIssue(p, true), status("commented on #%d", v.number))
}

// issueEdited takes the comment back from the editor. An editor left empty
// empties the field, the way it does the prompt.
func (m *Model) issueEdited(msg issueEditedMsg) tea.Cmd {
	defer os.Remove(msg.path)
	_, v := m.issueOf(msg.id)
	if v == nil {
		return nil
	}
	if msg.err != nil {
		return m.report(true, "editor exited: %v", msg.err)
	}
	edited, err := os.ReadFile(msg.path)
	if err != nil {
		return m.report(true, "cannot read the comment back: %v", err)
	}
	v.composer.SetValue(strings.TrimRight(string(edited), "\n"))
	v.composing = true
	return v.composer.Focus()
}

// issueKey is a key typed at an issue tab. The leader and what follows it are
// the pane's key map, as in any tab; the rest are the issue's.
func (m *Model) issueKeyPress(p *pane, msg tea.KeyPressMsg) tea.Cmd {
	if p.term.PrefixPending() || keyName(msg) == m.leader() {
		term, cmd := p.term.Update(msg)
		p.term = term
		return fromPane(p.id, cmd)
	}
	v := p.issue
	if v.composing {
		return m.composeKey(p, msg)
	}
	if v.files && v.filesKey(keyName(msg)) {
		return nil
	}
	switch keyName(msg) {
	case "up", "k":
		v.scroll(-1)
	case "down", "j":
		v.scroll(1)
	case "pgup", "b":
		v.scroll(-max(v.page-1, 1))
	case "pgdown", " ":
		v.scroll(max(v.page-1, 1))
	case "home", "g":
		v.shown().offset = 0
	case "end", "G":
		v.scroll(len(v.shown().lines))
	case "f":
		if v.issue == nil || v.issue.Pull == nil {
			return nil
		}
		v.files = !v.files
	case "c":
		if v.files {
			// The files changed are read, not answered: f goes back to
			// where the answering is.
			return nil
		}
		if v.issue == nil {
			return status("the %s has not been read yet", v.noun())
		}
		v.composing = true
		return v.composer.Focus()
	case "r":
		if v.loading {
			return nil
		}
		// Asked for: what it brings is what the person has now seen, a head
		// newer than the last one included. The notice goes when the read
		// lands, not before: one that fails has shown them nothing new.
		v.asked = true
		return tea.Batch(m.loadIssue(p, true), status("reading #%d again", v.number))
	case "x":
		if v.files {
			return nil
		}
		return m.askIssueState(p)
	case pullMergeKey:
		if v.files || !v.pull {
			return nil
		}
		return m.askMerge(p)
	case "o":
		return m.openLink(v.pageURL())
	case "y":
		return m.copyText(v.pageURL(), "copied "+v.pageURL())
	case "q":
		m.closeTab(p)
	}
	return nil
}

// askIssueState is x on an issue tab: an open issue is closed — as done, or
// as not planned, the page's two ways — and a closed one reopened. A pull
// request closes one way, unmerged, and a merged one is past both. It is
// asked rather than done on the key: it is public, and everyone watching the
// issue hears about it.
func (m *Model) askIssueState(p *pane) tea.Cmd {
	v := p.issue
	switch {
	case v.issue == nil:
		return status("the %s has not been read yet", v.noun())
	case v.changing:
		return nil
	case v.issue.Pull != nil && v.issue.Pull.Merged:
		return status("%s is merged — there is nothing to close or reopen", v.label())
	case v.issue.State == "closed":
		reason := "reopened"
		if v.pull {
			reason = ""
		}
		d := confirmDialog("Reopen #"+itoa(v.number)+"?", v.issue.Title, func(string) tea.Cmd {
			return m.setIssueState(p, "open", reason)
		})
		d.over = p
		m.dialog = d
		return nil
	case v.pull:
		d := confirmDialog("Close #"+itoa(v.number)+" without merging it?", v.issue.Title, func(string) tea.Cmd {
			return m.setIssueState(p, "closed", "")
		})
		d.over = p
		m.dialog = d
		return nil
	}
	items := []action{
		{key: "completed", press: "c", label: "close as completed", detail: "done, fixed, or answered", enabled: true},
		{key: "not_planned", press: "n", label: "close as not planned", detail: "won't fix, can't reproduce, duplicate, or stale", enabled: true},
	}
	d := actionsDialog("Close #"+itoa(v.number), v.issue.Title, items, func(reason string) tea.Cmd {
		return m.setIssueState(p, "closed", reason)
	})
	d.over = p
	m.dialog = d
	return nil
}

// setIssueState sends a close or a reopen.
func (m *Model) setIssueState(p *pane, state, reason string) tea.Cmd {
	v := p.issue
	v.changing = true
	ctx, ds, id, repository, number := m.ctx, m.ds, p.id, v.repository, v.number
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, issueTimeout)
		defer cancel()
		return issueStateMsg{id: id, state: state, reason: reason, err: ds.SetIssueState(ctx, repository, number, state, reason)}
	}
}

// issueStateSet reads the issue again once GitHub has it, so the badge and
// the timeline say what was done.
func (m *Model) issueStateSet(msg issueStateMsg) tea.Cmd {
	p, v := m.issueOf(msg.id)
	if v == nil {
		return nil
	}
	v.changing = false
	if msg.err != nil {
		return m.report(true, "#%d was not %s: %v", v.number, stateVerb(msg.state), msg.err)
	}
	v.toEnd, v.files = true, false
	said := "reopened #" + itoa(v.number)
	if msg.state == "closed" {
		said = "closed #" + itoa(v.number)
		if msg.reason != "" {
			said += " as " + strings.ReplaceAll(msg.reason, "_", " ")
		}
	}
	return tea.Batch(m.loadIssue(p, true), status("%s", said))
}

func stateVerb(state string) string {
	if state == "closed" {
		return "closed"
	}
	return "reopened"
}

// composeKey is a key while the comment is being written. Every key is the
// field's but the three that leave it: post, the editor, and Esc, which steps
// out and keeps what was written.
func (m *Model) composeKey(p *pane, msg tea.KeyPressMsg) tea.Cmd {
	v := p.issue
	switch keyName(msg) {
	case issuePostKey:
		return m.postComment(p)
	case "esc":
		v.composing = false
		v.composer.Blur()
		return nil
	case "alt+e", "f2":
		if v.posting {
			// What comes back from the editor replaces the field, and the
			// post landing clears it: one of them would lose the other.
			return status("the comment is still posting")
		}
		id := p.id
		return editText(m.ctx, v.composer.Value(), "comment", func(path string, err error) tea.Msg {
			return issueEditedMsg{id: id, path: path, err: err}
		})
	}
	if v.posting {
		// The text on its way to GitHub is the text on screen until it
		// lands; an edit under it would be lost or posted, and neither is
		// what was typed.
		return nil
	}
	var cmd tea.Cmd
	v.composer, cmd = v.composer.Update(msg)
	return cmd
}

// pasteIssue is a paste into an issue tab: into the comment, when one is being
// written, and nowhere otherwise.
func (m *Model) pasteIssue(p *pane, msg tea.PasteMsg) tea.Cmd {
	v := p.issue
	if !v.composing || v.posting {
		return nil
	}
	var cmd tea.Cmd
	v.composer, cmd = v.composer.Update(msg)
	return cmd
}

// scroll moves the page on screen, held between its top and its last screen.
func (v *issuePane) scroll(delta int) {
	shown := v.shown()
	shown.offset = min(max(shown.offset+delta, 0), max(len(shown.lines)-v.page, 0))
}

// pageURL is what o opens: the page on GitHub that the tab is showing.
func (v *issuePane) pageURL() string {
	if v.files {
		return v.url + "/files"
	}
	return v.url
}

// issueHints are the keys on an issue tab.
func issueHints(v *issuePane) []hint {
	if v.composing {
		return []hint{
			keyed("Ctrl-S", issuePostKey, "post"),
			says("Enter newline"),
			keyed("Alt-E", "alt+e", "editor"),
			keyed("Esc", "esc", "stop writing"),
		}
	}
	verb := "comment"
	if strings.TrimSpace(v.composer.Value()) != "" {
		verb = "go on writing"
	}
	what := "issue"
	if v.pull {
		what = "PR"
	}
	hints := []hint{says("↑↓ scroll")}
	pull := v.issue != nil && v.issue.Pull != nil
	switch {
	case pull && v.files:
		// The diff's own keys in place of the conversation's: nothing is
		// answered from here.
		if v.fileOpen {
			hints = append(hints, keyed("n", "n", "next file"), keyed("p", "p", "previous"), keyed("Esc", "esc", "files"))
		} else {
			hints[0] = says("↑↓ move")
			hints = append(hints, keyed("Enter", "enter", "diff"))
		}
		hints = append(hints, keyed("f", "f", "conversation"))
	case pull:
		if v.issue.State == "open" {
			hints = append(hints, keyed(pullMergeKey, pullMergeKey, "merge"))
		}
		hints = append(hints, keyed("f", "f", "files changed"), keyed("c", "c", verb))
	default:
		hints = append(hints, keyed("c", "c", verb))
	}
	switch {
	case v.files || pull && v.issue.Pull.Merged:
	case v.issue != nil && v.issue.State == "closed":
		hints = append(hints, keyed("x", "x", "reopen "+what))
	default:
		hints = append(hints, keyed("x", "x", "close "+what))
	}
	return append(hints,
		keyed("o", "o", "open on GitHub"),
		keyed("r", "r", "reload"),
		keyed("q", "q", "close tab"),
	)
}

// issueView is an issue tab's grid, width by height: the issue scrolled to
// where it is being read, and under it the comment field while one is being
// written.
func (m *Model) issueView(v *issuePane, width, height int) []string {
	v.mergeAt = -1
	var foot []string
	if v.composing && height > issueComposerRows+4 {
		v.composer.SetWidth(width)
		v.composer.SetHeight(issueComposerRows)
		label := " Comment on #" + itoa(v.number) + " "
		if v.posting {
			label = " Posting… "
		}
		foot = append(foot, m.st.frame.Render("──"+label+strings.Repeat("─", max(width-2-lipgloss.Width(label), 0))))
		foot = append(foot, strings.Split(v.composer.View(), "\n")...)
	} else if v.composing {
		// Too short for a field and the issue both: the field wins, since it
		// has the keys.
		v.composer.SetWidth(width)
		v.composer.SetHeight(max(height, 1))
		return fitRows(strings.Split(v.composer.View(), "\n"), height)
	}

	var head []string
	if v.err != "" {
		say := "could not read the " + v.noun() + ": "
		if v.issue != nil {
			say = "could not read it again: "
		}
		head = append(head, wrapLines(m.st.statusER.Render(say+v.err), width)...)
	}
	if v.pushed != "" && v.issue != nil && v.issue.Pull != nil {
		head = append(head, wrapLines(m.st.statusWA.Render(pushedNotice(v.pushed, v.issue.Pull.HeadSHA)+" — r reads it again"), width)...)
	}

	room := max(height-len(foot)-len(head), 0)
	if v.issue == nil {
		body := head
		if v.err == "" {
			body = append(body, m.st.dimText.Render(truncate("reading "+v.url+" …", width)))
		}
		return append(fitRows(body, height-len(foot)), foot...)
	}
	shown := v.shown()
	if shown.lines == nil || shown.width != width {
		switch {
		case v.files && v.fileOpen:
			shown.lines = m.pullDiffLines(*v.issue, v.fileCursor, width)
		case v.files:
			shown.lines, v.fileRow = m.pullFileLines(*v.issue, v.fileCursor, width)
		default:
			shown.lines = m.issueLines(v, *v.issue, width, time.Now())
		}
		shown.width = width
	}
	v.page = room
	if v.toEnd {
		v.conversation.offset, v.toEnd = len(v.conversation.lines), false
	}
	if v.files && !v.fileOpen {
		// The cursor is kept on screen, as a list keeps it. On the first file
		// that is the top of the page, with the heading over the list, and on
		// the last its end, with whatever is said under it.
		row := v.fileRow + v.fileCursor
		low, high := row-room+1, row
		if v.fileCursor == 0 {
			high = 0
		}
		if v.fileCursor >= len(v.issue.Pull.Files)-1 {
			low = max(low, min(len(shown.lines)-room, row))
		}
		shown.offset = max(min(shown.offset, high), low)
	}
	v.scroll(0)
	v.fileTop = len(head) + v.fileRow - shown.offset
	// Not while a comment is being written: the keys are the field's then, and
	// a press of this one would type into it.
	if !v.files && !v.composing && v.mergeRow >= shown.offset && v.mergeRow < shown.offset+room {
		v.mergeAt = len(head) + v.mergeRow - shown.offset
	}
	end := min(shown.offset+room, len(shown.lines))
	body := append(head, shown.lines[shown.offset:end]...)
	return append(fitRows(body, height-len(foot)), foot...)
}

// drawnLink is one OSC 8 link on a drawn line: where it starts, in cells, and
// how wide it is, and where its text is in the line's bytes.
type drawnLink struct {
	url      string
	x, width int
	from, to int
}

// osc8 is an OSC 8 hyperlink sequence, opening (with a URL) or closing (with
// none), ended by ST or BEL.
var osc8 = regexp.MustCompile("\x1b\\]8;[^;\x07\x1b]*;([^\x07\x1b]*)(?:\x07|\x1b\\\\)")

// lineLinks finds the links on a line as it is drawn — glamour draws a
// comment's links as OSC 8 — measuring the text between them in cells, so the
// marks land where the text does.
func lineLinks(line string) []drawnLink {
	var links []drawnLink
	open, start, from, x := "", 0, 0, 0
	end := func(to int) {
		if open != "" && x > start {
			links = append(links, drawnLink{url: open, x: start, width: x - start, from: from, to: to})
		}
	}
	at := 0
	for _, loc := range osc8.FindAllStringSubmatchIndex(line, -1) {
		x += ansi.StringWidth(line[at:loc[0]])
		end(loc[0])
		open, start, from = line[loc[2]:loc[3]], x, loc[1]
		at = loc[1]
	}
	x += ansi.StringWidth(line[at:])
	end(len(line))
	return links
}

// webLinksOnly takes every link out of a rendered line but the web ones,
// leaving their text. A stranger writes these: unmarked, a file:// or a custom
// scheme would still be an OSC 8 the terminal's own Ctrl-click follows.
func webLinksOnly(line string) string {
	return osc8.ReplaceAllStringFunc(line, func(seq string) string {
		target := osc8.FindStringSubmatch(seq)[1]
		if target == "" || webLink(target) {
			return seq
		}
		return ""
	})
}

// litLink redraws one link on a line in the hover style, from its bare text,
// leaving the sequences round it: the pointer is on it.
func litLink(st *styles, line string, link drawnLink) string {
	return line[:link.from] + st.hover.Render(ansi.Strip(line[link.from:link.to])) + line[link.to:]
}

// webLink reports whether a link in an issue is an absolute http or https
// URL, the only kind a click on one opens.
func webLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// fitRows pads or cuts rows to exactly n.
func fitRows(rows []string, n int) []string {
	n = max(n, 0)
	if len(rows) > n {
		return rows[:n]
	}
	for len(rows) < n {
		rows = append(rows, "")
	}
	return rows
}

// wrapLines wraps text to width and cuts what still does not fit — a URL with
// no place to break, a code line — rather than letting it run past the border.
func wrapLines(text string, width int) []string {
	lines := strings.Split(ansi.Wrap(text, width, ""), "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, width, "…")
	}
	return lines
}

// issueLines is the whole issue as the page lays it out, at width: the title
// and the state, the facts the page's sidebar has, the description, and the
// timeline.
func (m *Model) issueLines(v *issuePane, issue Issue, width int, now time.Time) []string {
	st := m.st
	var out []string

	// The title is a title: the accent and bold, as on every card and dialog.
	title := st.dialogTitle.Render(issue.Title) + " " + st.dimText.Render("#"+itoa(issue.Number))
	out = append(out, wrapLines(title, width)...)
	opened := "opened this issue"
	if issue.PullRequest {
		opened = "opened this pull request"
	}
	byline := issueState(st, issue) + "  " + st.emphasis.Render(issue.Author) + " " +
		st.dimText.Render(opened+" "+ago(issue.CreatedAt, now)+" · "+plural(issue.Comments, "comment", "comments"))
	if issue.Pull != nil {
		byline = pullByline(st, issue, now)
	}
	out = append(out, wrapLines(byline, width)...)
	out = append(out, "")
	v.mergeRow = -1
	if banner := mergeBanner(st, issue, width); banner != "" {
		if pullReady(issue) && mergeBandKeyFits(width) {
			v.mergeRow = len(out)
		}
		out = append(out, banner, "")
	}

	factRows := func(label string, rows []string) {
		lead := st.dimText.Render(pad(label, issueLabelWidth))
		for i, row := range rows {
			if i > 0 {
				lead = strings.Repeat(" ", issueLabelWidth)
			}
			out = append(out, lead+row)
		}
	}
	fact := func(label, value string) { factRows(label, wrapLines(value, max(width-issueLabelWidth, 1))) }
	if issue.Pull != nil {
		factRows("Reviewers", reviewerRows(st, issue.Pull.Reviewers, max(width-issueLabelWidth, 1)))
	}
	if len(issue.Assignees) > 0 {
		fact("Assignees", strings.Join(issue.Assignees, ", "))
	}
	if len(issue.Labels) > 0 {
		// Packed a chip at a time, so a row breaks between labels and never
		// inside one: a chip is one thing to read.
		room := max(width-issueLabelWidth, 1)
		var rows []string
		for _, label := range issue.Labels {
			chip := truncate(labelChip(st, label), room)
			last := len(rows) - 1
			if last >= 0 && lipgloss.Width(rows[last])+2+lipgloss.Width(chip) <= room {
				rows[last] += "  " + chip
				continue
			}
			rows = append(rows, chip)
		}
		factRows("Labels", rows)
	}
	if issue.Type != "" {
		fact("Type", issue.Type)
	}
	if issue.Milestone != "" {
		fact("Milestone", issue.Milestone)
	}
	if issue.Locked {
		fact("Locked", "the conversation is limited to collaborators")
	}
	if issue.SubIssuesTotal > 0 || len(issue.SubIssues) > 0 {
		// GitHub's own total: the list may have been cut short, and the
		// count done is GitHub's too.
		total := max(issue.SubIssuesTotal, len(issue.SubIssues))
		summary := fmt.Sprintf("%d of %d done", issue.SubIssuesDone, total)
		if len(issue.SubIssues) < total {
			summary += fmt.Sprintf(" · %d shown", len(issue.SubIssues))
		}
		fact("Sub-issues", summary)
		for _, sub := range issue.SubIssues {
			out = append(out, truncate(strings.Repeat(" ", issueLabelWidth)+refText(st, sub, issue.Repository), width))
		}
	}

	if pull := issue.Pull; pull != nil {
		room := max(width-issueLabelWidth, 1)
		fact("Changes", pullChanges(st, *pull))
		factRows("Checks", checkRows(st, pull.Checks, pull.ChecksPartial, room))
	}

	out = append(out, m.commentLines(v, issue.Author, "opened", issue.CreatedAt, issue.Body, issue.Reactions, width, now)...)
	// What happened between the comments is a table, as the audit screen's
	// records are: when, who, and what, each in its own column, a run of
	// events one row under another.
	who := 0
	for _, event := range issue.Timeline {
		if event.Kind != "commented" && event.Kind != "reviewed" {
			who = max(who, lipgloss.Width(event.Actor))
		}
	}
	who = min(who, eventWhoWidth)
	var run []eventRow
	flush := func() {
		if len(run) > 0 {
			out = append(out, "")
			out = append(out, eventRows(st, run, who, width, now)...)
			run = nil
		}
	}
	for i, event := range issue.Timeline {
		switch {
		case event.Kind == "commented":
			flush()
			out = append(out, m.commentLines(v, event.Actor, "commented", event.At, event.Body, event.Reactions, width, now)...)
			continue
		case event.Kind == "reviewed":
			flush()
			out = append(out, m.reviewLines(v, event, width, now)...)
			continue
		case event.Kind == "committed":
			// A pushed commit: its short name, dim, and what it says.
			run = append(run, eventRow{at: event.At, who: event.Actor, what: st.dimText.Render(event.Subject) + " " + event.Message})
			continue
		case event.Kind == "closed" && issue.Pull != nil && i > 0 && issue.Timeline[i-1].Kind == "merged":
			// Merging closes it, and the page says the one thing.
			continue
		}
		said := eventText(st, event, issue.Repository)
		if issue.Pull != nil {
			said = pullEventText(st, event, issue)
		}
		run = append(run, eventRow{at: event.At, who: event.Actor, what: said})
	}
	flush()
	if issue.Truncated {
		out = append(out, "", st.statusWA.Render(truncate("the timeline goes on — o opens the rest on GitHub", width)))
	}
	return out
}

// groupBar is the head of one thing in a page of them — a comment, a review, a
// file's diff — drawn the way the list heads a group of discoboxes: a bar on
// the group field, what it is on the left and the fact about it on the right.
// A blank row comes with it, above. Without color there is no field, so a rule
// does the separating.
func groupBar(st *styles, left, right string, width int) []string {
	room := max(width-2, 1)
	left = truncate(left, max(room-lipgloss.Width(right)-1, 1))
	row := " " + padANSI(spread(left, right, room), room) + " "
	if !st.color {
		return []string{"", st.rule.Render(strings.Repeat("─", width)), strings.TrimRight(row, " ")}
	}
	return []string{"", highlight(st, row, colInactive)}
}

// eventRow is one thing that happened in a timeline: when, who did it, and
// what, which may carry its own styling.
type eventRow struct {
	at   time.Time
	who  string
	what string
}

const (
	// eventTimeWidth is the timeline's time column: "12mo ago" and a gap.
	eventTimeWidth = 10
	// eventWhoWidth is the most the who column takes: a login is 39 at the
	// longest, and one that long gives way before what it did does.
	eventWhoWidth = 16
)

// eventRows lays a run of events out in columns: the time dim and first, as
// the audit screen has it, then who, who cells wide — one width for the whole
// page, so a run lines up with the runs above and below it — then what. What does not fit on the row runs on under its own column.
func eventRows(st *styles, run []eventRow, who, width int, now time.Time) []string {
	lead := eventTimeWidth + who + 2
	if who == 0 {
		lead = eventTimeWidth
	}
	room := max(width-lead, 1)
	var out []string
	for _, row := range run {
		head := st.dimText.Render(pad(ago(row.at, now), eventTimeWidth))
		if who > 0 {
			head += pad(truncate(row.who, who), who) + "  "
		}
		for i, line := range wrapLines(row.what, room) {
			if i > 0 {
				head = strings.Repeat(" ", lead)
			}
			out = append(out, head+line)
		}
	}
	return out
}

// commentLines is one comment — the description is the first — under a bar
// with who wrote it and when, rendered as GitHub renders its markdown, with
// the reactions under it.
func (m *Model) commentLines(v *issuePane, author, verb string, at time.Time, body string, reactions IssueReactions, width int, now time.Time) []string {
	st := m.st
	out := groupBar(st, st.attentionText.Render(author)+" "+st.attentionHint.Render(verb), st.attentionHint.Render(ago(at, now)), width)
	if strings.TrimSpace(body) == "" {
		return append(out, st.dimText.Render("No description provided."))
	}
	out = append(out, m.markdown(v, body, width)...)
	if len(reactions) > 0 {
		out = append(out, "", reactionText(st, reactions))
	}
	return out
}

// markdown renders a comment's markdown at width, the way the page renders
// it, falling back to the text wrapped when it will not render. The blank
// lines the renderer puts round a document are the page's; here the comment's
// own rule already separates it.
func (m *Model) markdown(v *issuePane, body string, width int) []string {
	if v.md[width] == nil {
		renderer, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle(m.st)), glamour.WithWordWrap(width), glamour.WithEmoji())
		if err != nil {
			return wrapLines(body, width)
		}
		if v.md == nil || len(v.md) >= issueRenderers {
			// A resize leaves a renderer behind for every width it passed
			// through.
			v.md = map[int]*glamour.TermRenderer{}
		}
		v.md[width] = renderer
	}
	rendered, err := v.md[width].Render(markdownSource(strings.ReplaceAll(body, "\r\n", "\n")))
	if err != nil {
		return wrapLines(body, width)
	}
	lines := strings.Split(strings.Trim(rendered, "\n"), "\n")
	for i, line := range lines {
		lines[i] = ansi.Truncate(strings.TrimRight(webLinksOnly(line), " "), width, "…")
	}
	return lines
}

// issueState is the badge the page draws beside the title: open, closed as
// done, or closed as something else.
func issueState(st *styles, issue Issue) string {
	switch {
	case issue.Pull != nil:
		return pullState(st, issue)
	case issue.State == "open":
		return st.stateRun.Render("● Open")
	case issue.StateReason == "not_planned":
		return st.stateOff.Render("○ Closed as not planned")
	case issue.StateReason == "duplicate":
		return st.stateOff.Render("○ Closed as duplicate")
	default:
		return st.stateOff.Render("○ Closed")
	}
}

// labelChip is a label: a dot in the color its page draws it in, and its
// name in the window's own text. The page paints the whole label, but nothing
// in this window is a painted field except a title bar and the row under the
// cursor, and half of GitHub's label colors cannot be read as text on a dark
// terminal. Without color it is bracketed, so one label ends where the next
// begins.
func labelChip(st *styles, label IssueLabel) string {
	if !st.color {
		return "[" + label.Name + "]"
	}
	dot := st.dimText
	if _, err := strconv.ParseUint(label.Color, 16, 32); err == nil && len(label.Color) == 6 {
		dot = lipgloss.NewStyle().Foreground(lipgloss.Color("#" + label.Color))
	}
	return dot.Render("●") + " " + label.Name
}

// reactionOrder is the page's order for the reactions, which are drawn by
// GitHub's names for them: an emoji is two cells in one terminal and one in
// the next, and nothing else in the window draws one.
var reactionOrder = []string{"+1", "-1", "laugh", "hooray", "confused", "heart", "rocket", "eyes"}

func reactionText(st *styles, reactions IssueReactions) string {
	var parts []string
	for _, name := range reactionOrder {
		if n := reactions[name]; n > 0 {
			parts = append(parts, name+" "+itoa(n))
		}
	}
	return st.dimText.Render(strings.Join(parts, " · "))
}

// refText is another issue or pull request as a line names it: its state,
// its number — with its repository when that is not this one — and its title.
func refText(st *styles, ref IssueRef, repository string) string {
	glyph := st.stateRun.Render("●")
	switch {
	case ref.Merged:
		glyph = st.info.Render("●")
	case ref.State == "closed":
		glyph = st.stateOff.Render("○")
	}
	number := "#" + itoa(ref.Number)
	if ref.Repository != "" && ref.Repository != repository {
		number = ref.Repository + number
	}
	return glyph + " " + st.dimText.Render(number) + " " + ref.Title
}

// eventText is what the page says an event did, after the name of who did
// it. A kind it has no words for is named as GitHub names it.
func eventText(st *styles, e IssueEvent, repository string) string {
	label := ""
	if e.Label != nil {
		label = e.Label.Name
	}
	switch e.Kind {
	case "labeled":
		return "added the " + label + " label"
	case "unlabeled":
		return "removed the " + label + " label"
	case "assigned":
		if e.Subject == e.Actor {
			return "self-assigned this"
		}
		return "assigned " + e.Subject
	case "unassigned":
		if e.Subject == e.Actor {
			return "removed their assignment"
		}
		return "unassigned " + e.Subject
	case "milestoned":
		return "added this to the " + e.Subject + " milestone"
	case "demilestoned":
		return "removed this from the " + e.Subject + " milestone"
	case "renamed":
		return "changed the title from “" + e.From + "” to “" + e.To + "”"
	case "closed":
		how := "closed this as completed"
		switch e.StateReason {
		case "not_planned":
			how = "closed this as not planned"
		case "duplicate":
			how = "closed this as a duplicate"
		}
		if e.Subject != "" {
			how += " in " + e.Subject
		}
		return how
	case "reopened":
		return "reopened this"
	case "locked":
		if e.Subject != "" {
			return "locked this as " + strings.ReplaceAll(e.Subject, "_", " ") + " and limited the conversation"
		}
		return "locked and limited the conversation"
	case "unlocked":
		return "unlocked this conversation"
	case "referenced":
		return "referenced this in commit " + e.Subject
	case "cross-referenced":
		if e.Source == nil {
			return "mentioned this"
		}
		return "mentioned this in " + ansi.Strip(refText(st, *e.Source, repository))
	case "pinned":
		return "pinned this issue"
	case "unpinned":
		return "unpinned this issue"
	case "transferred":
		return "transferred this issue"
	case "marked_as_duplicate":
		return "marked this as a duplicate"
	case "unmarked_as_duplicate":
		return "marked this as not a duplicate"
	case "connected":
		return "linked a pull request that will close this issue"
	case "disconnected":
		return "removed a link to a pull request"
	case "sub_issue_added":
		return "added a sub-issue"
	case "sub_issue_removed":
		return "removed a sub-issue"
	case "parent_issue_added":
		return "added a parent issue"
	case "parent_issue_removed":
		return "removed a parent issue"
	case "issue_type_added", "issue_type_changed":
		return "set the issue type"
	case "issue_type_removed":
		return "removed the issue type"
	}
	return strings.ReplaceAll(e.Kind, "_", " ")
}
