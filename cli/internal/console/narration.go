package console

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// A running command's channel back to the console (ADR 26-11-07-876 §1,
// 26-11-07-289 §2): what it reports, what it asks, and the terminal it needs.
// The command writes; the frontend that started it reads Events until it is
// closed, and answers what is asked with its own dialogs. Nothing the command
// is handed is a frontend's function, so a frontend over the bridge answers
// the same values the TUI does.

// Progress is one thing a running command says: a step of its work, or a
// warning about the work that is no step of it — uncommitted changes it left
// out, a settings file it could not read all of — which a frontend keeps on
// screen past the steps that follow.
type Progress struct {
	Text    string
	Warning bool
}

// Question is what a running command, or a command before it runs, asks. With
// Choices it is a pick of one, each Choice's Hint what choosing it means; with
// none it is a yes or no, answered AnswerYes or AnswerNo — and a yes or no
// dismissed is answered no, since that is what leaving it means.
type Question struct {
	Title string
	// Subject is what the question is about, said on a line of its own above
	// the body: the directory a create would carry.
	Subject string
	Body    string
	// Measure is a directory whose size the answer turns on: the console
	// counts it while the question is up (DataSource.MeasureDirectory) and
	// says the count as it climbs, since waiting for the number before asking
	// would be the wait the question exists to avoid. Only the copy question
	// has one.
	Measure string
	// Choices are the answers to pick from, by Key.
	Choices []Choice
	// DefaultNo is a yes or no whose quick answer is no: a destruction
	// nobody can undo has to be chosen, not fallen into by answering the way
	// every other question is answered.
	DefaultNo bool
}

// The answers to a yes or no.
const (
	AnswerYes = "yes"
	AnswerNo  = "no"
)

// ErrNoFrontend is the answer to a question nobody can be asked: a command
// started with no narration, or a frontend that cannot ask it.
var ErrNoFrontend = errors.New("nothing can answer this here")

// Event is one thing a Narration delivers: exactly one field is set.
type Event struct {
	// Progress is a step or a warning. A frontend that falls behind loses
	// steps, never the other events.
	Progress *Progress
	// Sending is a create past its questions (Create), on its way to the
	// server: nothing the create asks from here is the create's own, and a
	// frontend that leaves it now leaves it to go on.
	Sending bool
	// Accepted is the box a create made, the moment the server took it:
	// from then on it exists whatever the rest of the run does.
	Accepted *Sandbox
	// Asking is a question the command waits on.
	Asking *Asking
	// Handover is a program the command needs the terminal for.
	Handover *Handover
	// Editing is a file the command needs edited in the person's editor.
	Editing *Editing
}

// Asking is a question in flight. The frontend answers it exactly once: with
// Answer, or Decline when it is taken down unanswered or cannot be asked.
type Asking struct {
	Question
	answered chan answer
}

type answer struct {
	key string
	err error
}

// Answer gives the question's answer, by Choice key (AnswerYes or AnswerNo for
// a yes or no).
func (a *Asking) Answer(key string) { a.send(answer{key: key}) }

// Decline leaves the question unanswered, saying why: context.Canceled for a
// question dismissed, ErrNoFrontend for one nothing can ask.
func (a *Asking) Decline(err error) { a.send(answer{err: err}) }

func (a *Asking) send(ans answer) {
	select {
	case a.answered <- ans:
	default:
	}
}

// Handover is a program the command runs with the terminal: Git's credential
// helper prompting for what no helper answered. The frontend runs Command with
// the terminal its own and says how it ended with Done, exactly once; one with
// no terminal to give says Done(ErrNoFrontend).
type Handover struct {
	Command *exec.Cmd
	done    chan error
}

// Done says how the program ended, or why it was never run.
func (h *Handover) Done(err error) {
	select {
	case h.done <- err:
	default:
	}
}

// Editing is a file on this machine the command needs edited: one of a
// harness's files written out, or a tool's copy itself, for the person's
// editor. Its path keeps whatever name it has, a tool declaration's among
// them: a frontend that opens a file by its type, or whose person can give
// an edit up after their editor saved, hands its editor a copy under
// EditExtension and writes back what was saved once done (the desktop
// bridge's Server.edit), never the path as it is. The frontend opens Path in the editor it opens files in —
// the TUI hands the terminal to $EDITOR, the desktop app opens it in the
// person's own — and says Done exactly once, when the person is finished
// with it: nil once it is saved or left, context.Canceled for an edit the
// person gave up, or why it could not be opened. The command reads the file
// back itself.
type Editing struct {
	Path string
	// Title is what the edit is, as the frontend names it while it waits.
	Title string
	done  chan error
}

// Done says the person is finished with the file, or why it was never edited.
func (e *Editing) Done(err error) {
	select {
	case e.done <- err:
	default:
	}
}

// narrationCapacity is how many events may wait unread. A command reports one
// step at a time, but a pull reports twice a second and a frontend may be
// mid-render, so there is room for a short burst before a step is dropped.
const narrationCapacity = 8

// Narration is one running command's feed. A nil Narration is a command
// nobody is watching: its reports go nowhere and its questions are declined
// with ErrNoFrontend.
type Narration struct {
	events chan Event
}

// NewNarration opens a feed for a command about to start. Whoever opens it
// reads Events until Close, which the command's starter calls once the command
// has returned.
func NewNarration() *Narration {
	return &Narration{events: make(chan Event, narrationCapacity)}
}

// Events is what the command says, in order, closed once it has finished.
func (n *Narration) Events() <-chan Event { return n.events }

// Close ends the feed. The command's starter calls it once the command has
// returned, and never twice.
func (n *Narration) Close() { close(n.events) }

// Report says a step of the work as it begins. It never blocks: a status line
// must not be able to stall the work it describes, so a step the frontend has
// no room for is dropped.
func (n *Narration) Report(text string) { n.progress(Progress{Text: text}) }

// Warn says what is wrong with the work without being a step of it, on the
// same terms as Report.
func (n *Narration) Warn(text string) { n.progress(Progress{Text: text, Warning: true}) }

func (n *Narration) progress(p Progress) {
	if n == nil {
		return
	}
	select {
	case n.events <- Event{Progress: &p}:
	default:
	}
}

// Sending says a create has asked all it asks (Create) and is being sent. It
// waits for room, as Accept does: what follows from it — the frontend's wait
// leaving the create to go on — is not a status line.
func (n *Narration) Sending(ctx context.Context) {
	if n == nil {
		return
	}
	select {
	case n.events <- Event{Sending: true}:
	case <-ctx.Done():
	}
}

// Accept says the server has taken a create and made box. It waits for room
// rather than being dropped, since what follows from it — letting the prompt
// go — is not a status line; and it gives up only when ctx does, so a create
// whose frontend stopped reading still returns once it is canceled.
func (n *Narration) Accept(ctx context.Context, box Sandbox) {
	if n == nil {
		return
	}
	select {
	case n.events <- Event{Accepted: &box}:
	case <-ctx.Done():
	}
}

// Ask puts q to the frontend and waits for its answer: the chosen key, or why
// there is none.
func (n *Narration) Ask(ctx context.Context, q Question) (string, error) {
	if n == nil {
		return "", ErrNoFrontend
	}
	asking := &Asking{Question: q, answered: make(chan answer, 1)}
	select {
	case n.events <- Event{Asking: asking}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case ans := <-asking.answered:
		return ans.key, ans.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Confirm asks a yes or no, and is whether the answer was yes.
func (n *Narration) Confirm(ctx context.Context, q Question) (bool, error) {
	key, err := n.Ask(ctx, q)
	return err == nil && key == AnswerYes, err
}

// Handover runs command with the frontend's terminal and waits for it to end.
func (n *Narration) Handover(ctx context.Context, command *exec.Cmd) error {
	if n == nil {
		return ErrNoFrontend
	}
	handover := &Handover{Command: command, done: make(chan error, 1)}
	select {
	case n.events <- Event{Handover: handover}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-handover.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Edit has the frontend open path in the person's editor and waits until they
// are finished with it.
func (n *Narration) Edit(ctx context.Context, path, title string) error {
	if n == nil {
		return ErrNoFrontend
	}
	editing := &Editing{Path: path, Title: title, done: make(chan error, 1)}
	select {
	case n.events <- Event{Editing: editing}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-editing.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Decline answers every question, handover and edit still to come on a feed whose
// frontend can ask none, and drops its progress, until the command closes it:
// a desktop verb before the app can draw a question.
func (n *Narration) Decline(why error) {
	for event := range n.events {
		switch {
		case event.Asking != nil:
			event.Asking.Decline(why)
		case event.Handover != nil:
			event.Handover.Done(why)
		case event.Editing != nil:
			event.Editing.Done(why)
		}
	}
}

// Confirmation is the question a lifecycle verb asks before it is sent, and
// whether it asks one. Purging asks, since nothing can bring the disk back;
// archiving is reversible and needs no ceremony, nor does anything else.
// Dispatch asks it on the way to Frontend.Confirm; anything sending a verb
// outside the registry refuses one that asks rather than skip the question.
func Confirmation(verb Verb, targets []Sandbox) (Question, bool) {
	if verb != VerbPurge {
		return Question{}, false
	}
	return Question{
		Title:     "Purge",
		Body:      "Purge " + TargetsTitle(targets) + "? The disk and everything on it goes, and unarchive cannot bring it back.",
		DefaultNo: true,
	}, true
}

// UpgradeOffer is what opening a box whose image is no longer available
// asks instead (Frontend.OfferUpgrade): whether to upgrade it to its
// harness's current image, a yes or no. A box whose harness has no newer
// image cannot be offered one, and ok is false: q is then the refusal, its
// title and why.
func UpgradeOffer(box Sandbox) (q Question, ok bool) {
	if !box.Upgrade {
		body := box.Name + "'s image is no longer available on its pool, so it cannot start, and its harness has no newer image to move it to."
		if box.Message != "" {
			body += "\n\n" + box.Message
		}
		return Question{Title: "Cannot start " + box.Name, Body: body}, false
	}
	return Question{
		Title: "Upgrade",
		Body:  box.Name + "'s image is no longer available on its pool, so it cannot start. Upgrade it to its harness's current image? Its work is kept.",
	}, true
}

// TargetsTitle names what a command acts on: the one box by name, or how many
// are selected.
func TargetsTitle(targets []Sandbox) string {
	if len(targets) == 1 {
		return targets[0].Name
	}
	return strconv.Itoa(len(targets)) + " boxes selected"
}

// editExtensions are the extensions a file handed to a frontend's editor
// keeps: text formats a desktop hands to a text editor. A harness file's path
// is its bundle's to choose and a tool file's name its declaration's (a
// discobox's image or source can declare one), and on Linux the desktop app
// opens a file with the handler for its type: an extension outside these (a
// script's, a launcher's, one carrying shell metacharacters) would let a box
// choose a program to run on this machine, and one a desktop shows rather
// than edits (.xml, .md, .html: a browser or a viewer) would leave nothing to
// save, so either is edited as .txt.
var editExtensions = map[string]bool{
	".json": true, ".jsonc": true, ".json5": true, ".toml": true, ".yaml": true, ".yml": true,
	".txt": true, ".ini": true, ".conf": true, ".cfg": true, ".env": true,
}

// EditExtension is the extension a file at path is edited under by an editor
// that opens files by their type: its own when it is one of editExtensions,
// else .txt.
func EditExtension(path string) string {
	if ext := strings.ToLower(filepath.Ext(path)); editExtensions[ext] {
		return ext
	}
	return ".txt"
}
