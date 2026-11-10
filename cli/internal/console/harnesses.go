package console

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The harnesses page's rules (ADR 26-11-07-876 §1): what can be done to one
// of the project's harnesses and why not, what resetting one asks first, and
// what its config card says. Each renderer draws them — the TUI's screen, the
// desktop's page — and binds its own keys to the actions; neither decides
// any of it.

// HarnessAction is one thing the harnesses page does to a harness.
type HarnessAction string

// The actions, in the order a harness's menu lists them.
const (
	// HarnessActionVariant creates a variant following the harness's bundle
	// with an account of its own (HarnessVariantForm).
	HarnessActionVariant HarnessAction = "variant"
	// HarnessActionDefault makes it the project's default.
	HarnessActionDefault HarnessAction = "default"
	// HarnessActionConfig shows its config card (HarnessCard).
	HarnessActionConfig HarnessAction = "config"
	// HarnessActionFiles edits one of its files in the person's editor
	// (DataSource.EditHarnessFile).
	HarnessActionFiles HarnessAction = "files"
	// HarnessActionReset resets it to its bundle's defaults, asking first
	// (Harness.ResetQuestion).
	HarnessActionReset HarnessAction = "reset"
)

// HarnessOffer is one action as a harness's menu offers it: what it is
// called, what it does, and why it cannot act when it cannot. One that does
// not apply stays on the menu with its reason.
type HarnessOffer struct {
	Action  HarnessAction
	Label   string
	Detail  string
	Enabled bool
	Why     string
}

// Actions is every action the harness's menu offers, each as it stands for
// it.
func (h Harness) Actions() []HarnessOffer {
	offer := func(a HarnessAction, label, detail string, enabled bool, why string) HarnessOffer {
		if enabled {
			why = ""
		}
		return HarnessOffer{Action: a, Label: label, Detail: detail, Enabled: enabled, Why: why}
	}
	return []HarnessOffer{
		offer(HarnessActionVariant, "create variant", "follow this bundle with an independent account", h.Parent == "",
			"a variant cannot be a parent; select its standalone parent instead"),
		offer(HarnessActionDefault, "default", "run it when a discobox says no harness", !h.Default, "already the default"),
		offer(HarnessActionConfig, "config", "everything the harness is set to", true, ""),
		offer(HarnessActionFiles, "files", "edit one of its files in your editor", len(h.Files) > 0, "it carries no files"),
		offer(HarnessActionReset, "reset", "forget saved settings and saved logins, back to the bundle's defaults", true, ""),
	}
}

// Offer is one action as the harness's menu offers it, and whether there is
// such an action.
func (h Harness) Offer(a HarnessAction) (HarnessOffer, bool) {
	for _, o := range h.Actions() {
		if o.Action == a {
			return o, true
		}
	}
	return HarnessOffer{}, false
}

// Refusal is what the offer says when it is taken and cannot act, about the
// harness by its name.
func (o HarnessOffer) Refusal(name string) string {
	return "cannot " + o.Label + " " + name + ": " + o.Why
}

// OfferedHarness reads a server's harnesses again and is the one an action
// names, refusing as its menu would: a harness no longer listed, an action it
// has no offer for, or one that does not apply, in the offer's words. Every
// console takes an action through it (or RunHarnessVerb, EditHarness, which
// do), never on the strength of a listing it read before.
func OfferedHarness(ctx context.Context, src DataSource, server, id string, a HarnessAction) (Harness, error) {
	listed, err := src.Harnesses(ctx, server)
	if err != nil {
		return Harness{}, fmt.Errorf("cannot list the harnesses: %w", err)
	}
	for _, h := range listed {
		if h.ID != id {
			continue
		}
		offer, ok := h.Offer(a)
		switch {
		case !ok:
			return Harness{}, fmt.Errorf("there is no harness action %q", a)
		case !offer.Enabled:
			return Harness{}, errors.New(offer.Refusal(h.DisplayName()))
		}
		return h, nil
	}
	return Harness{}, fmt.Errorf("no harness %s is listed", id)
}

// RunHarnessVerb runs a verb on a harness as its menu offers it — making it
// the default, or a reset answered — and is what the status line says of it.
func RunHarnessVerb(ctx context.Context, src DataSource, server, id string, verb HarnessVerb) (string, error) {
	action := HarnessActionReset
	if verb == HarnessSetDefault {
		action = HarnessActionDefault
	}
	h, err := OfferedHarness(ctx, src, server, id, action)
	if err != nil {
		return "", err
	}
	if err := src.DoHarness(ctx, server, verb, h.ID); err != nil {
		return "", fmt.Errorf("%s %s: %w", verb, h.DisplayName(), err)
	}
	return verb.Done(h.DisplayName()), nil
}

// EditHarness has one of a harness's files edited in the person's editor,
// through n, and saved back (DataSource.EditHarnessFile), refusing a harness
// whose files action does not apply and a path it does not carry; it is what
// the status line says of it. An edit given up is context.Canceled.
func EditHarness(ctx context.Context, src DataSource, server, id, path string, n *Narration) (string, error) {
	h, err := OfferedHarness(ctx, src, server, id, HarnessActionFiles)
	if err != nil {
		return "", err
	}
	if _, ok := h.File(path); !ok {
		return "", fmt.Errorf("%s carries no file %q", h.DisplayName(), path)
	}
	changed, err := src.EditHarnessFile(ctx, server, h.ID, path, n)
	switch {
	case err != nil:
		return "", fmt.Errorf("edit %s: %w", path, err)
	case !changed:
		return path + " unchanged", nil
	}
	return "updated " + path, nil
}

// ResetQuestion is what resetting the harness asks first. Reset deletes what
// saves from discoboxes put on it — settings files and the logins they
// created — and nothing brings those back but signing in and saving again.
// A harness with files a person added or overrode is a pick of one, keeping
// them first (ADR 26-11-04-711 §1): its choices are the verbs, by their
// names. One without is a yes or no. ResetVerb reads the answer.
func (h Harness) ResetQuestion() Question {
	body := fmt.Sprintf("Reset %s to its bundle's defaults? The settings and logins saved to it from discoboxes are deleted. New discoboxes on it start signed out until a login made in one is saved again; secrets you bound to it by hand are kept.",
		h.DisplayName())
	if !h.HasOverrides() {
		return Question{Title: "Reset", Body: body}
	}
	return Question{
		Title: "Reset",
		Body:  body + "\n\nIt has files you added or overrode yourself.",
		Choices: []Choice{
			{Key: string(HarnessReset), Label: "Reset, keep your files", Hint: "the files you added or overrode stay"},
			{Key: string(HarnessResetOverrides), Label: "Reset your files too", Hint: "nothing but the bundle's files is left"},
		},
	}
}

// ResetVerb is the verb an answer to ResetQuestion asks for, and false for
// an answer that resets nothing.
func (h Harness) ResetVerb(answer string) (HarnessVerb, bool) {
	if !h.HasOverrides() {
		return HarnessReset, answer == AnswerYes
	}
	switch v := HarnessVerb(answer); v {
	case HarnessReset, HarnessResetOverrides:
		return v, true
	}
	return "", false
}

// Notes are what the files picker and the card say about a file beside its
// size, which each renderer writes its own way: the set it comes from, the
// one it hides, and whether it is only ever created.
func (f HarnessFile) Notes() []string {
	var notes []string
	switch f.Source {
	case HarnessFileSaved:
		notes = append(notes, "saved from a discobox")
	case HarnessFileOverride:
		notes = append(notes, "yours")
	default:
		notes = append(notes, "from the image")
	}
	if len(f.Shadows) > 0 {
		switch f.Shadows[0] {
		case HarnessFileSaved:
			notes = append(notes, "over the saved one")
		default:
			notes = append(notes, "over the image's")
		}
	}
	if f.CreateOnly {
		notes = append(notes, "create-only")
	}
	return notes
}

// File is the harness's file at path, as a discobox is given it.
func (h Harness) File(path string) (HarnessFile, bool) {
	for _, f := range h.Files {
		if f.Path == path {
			return f, true
		}
	}
	return HarnessFile{}, false
}

// CardTone is how a renderer draws a stretch of a card's value, or a
// detail's field or line (DetailSection).
type CardTone string

// The tones.
const (
	// CardPlain is the value itself.
	CardPlain CardTone = "plain"
	// CardQuiet is said about the value, beside it.
	CardQuiet CardTone = "quiet"
	// CardStandout is the one word on the card that stands out: the default.
	CardStandout CardTone = "standout"
	// CardCommand is a command line.
	CardCommand CardTone = "command"
	// CardAlert is what is wrong: a credential sent nowhere, or one that
	// cannot renew itself.
	CardAlert CardTone = "alert"
	// CardGood is what an answer adds: a route a binding gains.
	CardGood CardTone = "good"
)

// CardSpan is a stretch of a card row's value, in its tone.
type CardSpan struct {
	Text string
	Tone CardTone
}

// CardRow is one row of a card: its label, empty where it carries on the
// row above's; its value as spans; and a file's contents under it, the
// first HarnessCardFileLines of them, with More how many lines were left
// out.
type CardRow struct {
	Label string
	Spans []CardSpan
	Lines []string
	More  int
}

// HarnessCardFileLines is how much of a file the card shows before saying
// how much is left. A card is something to glance at; a file long enough to
// page through is one to open in the editor.
const HarnessCardFileLines = 40

// HarnessCard is everything the harness is set to, as rows: what it is, what
// it runs, which secret answers each variable it needs (secrets, from
// DataSource.HarnessSecrets), the files it carries and when it last changed.
func HarnessCard(h Harness, secrets []HarnessSecret) []CardRow {
	plain := func(text string) CardSpan { return CardSpan{Text: text, Tone: CardPlain} }
	quiet := func(text string) CardSpan { return CardSpan{Text: text, Tone: CardQuiet} }
	var rows []CardRow
	add := func(label string, spans ...CardSpan) { rows = append(rows, CardRow{Label: label, Spans: spans}) }

	var role []CardSpan
	if h.Default {
		role = append(role, CardSpan{Text: "★ default", Tone: CardStandout})
	}
	if h.BuiltIn {
		if len(role) > 0 {
			role = append(role, plain("  "))
		}
		role = append(role, quiet("built-in"))
	}
	if len(role) > 0 {
		add("Role", role...)
	}
	if h.Slug != "" && h.Slug != h.DisplayName() {
		add("Run as", plain(h.Slug))
	}
	add("ID", plain(h.ID))
	if h.Bundle != "" {
		add("Bundle", plain(h.Bundle))
	}
	if h.Digest != "" {
		add("Digest", plain(h.Digest))
	}
	if len(h.Run) > 0 {
		add("Run", CardSpan{Text: strings.Join(h.Run, " "), Tone: CardCommand})
	}
	if len(h.Relaunch) > 0 {
		add("Relaunch", CardSpan{Text: strings.Join(h.Relaunch, " "), Tone: CardCommand})
	}
	label := "Secrets"
	for _, secret := range secrets {
		add(label, secret.spans()...)
		label = ""
	}
	label = "Files"
	for _, file := range h.Files {
		row := CardRow{Label: label, Spans: []CardSpan{plain(file.Path), quiet("  (" + strings.Join(file.Notes(), " · ") + ")")}}
		row.Lines, row.More = fileLines(file.Content)
		rows = append(rows, row)
		label = ""
	}
	if !h.Updated.IsZero() {
		add("Updated", plain(h.Updated.Format(time.RFC3339)))
	}
	return rows
}

// spans is one environment variable and whatever answers it.
func (s HarnessSecret) spans() []CardSpan {
	var notes []string
	if s.Required {
		notes = append(notes, "required")
	}
	if s.OneOf != "" {
		notes = append(notes, "one of "+s.OneOf)
	}
	if !s.Declared {
		notes = append(notes, "bound by hand")
	}
	out := []CardSpan{{Text: s.Name, Tone: CardPlain}}
	if len(notes) > 0 {
		out = append(out, CardSpan{Text: " (" + strings.Join(notes, ", ") + ")", Tone: CardQuiet})
	}
	if s.SecretID == "" {
		return append(out, CardSpan{Text: " → nothing bound", Tone: CardQuiet})
	}
	var about []string
	if s.SecretType != "" {
		about = append(about, s.SecretType)
	}
	if s.SecretName != "" && s.SecretName != s.SecretID {
		about = append(about, s.SecretName)
	}
	if s.Anonymous {
		about = append(about, "anonymous")
	}
	out = append(out, CardSpan{Text: " → " + s.SecretID, Tone: CardPlain})
	if len(about) > 0 {
		out = append(out, CardSpan{Text: " (" + strings.Join(about, ", ") + ")", Tone: CardQuiet})
	}
	return out
}

// fileLines is a file's contents as the card shows them: the first
// HarnessCardFileLines lines, and how many were left out.
func fileLines(content string) ([]string, int) {
	if strings.TrimSpace(content) == "" {
		return nil, 0
	}
	lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
	if len(lines) <= HarnessCardFileLines {
		return lines, 0
	}
	return lines[:HarnessCardFileLines], len(lines) - HarnessCardFileLines
}

// ---------------------------------------------------------------------------
// A box's harness changes (ADR 26-11-02-152): what a box captured of its
// harness's settings and logins, reviewed before it is saved for every box
// on the harness.

// HarnessChange is what a review of a box's harness changes does with the
// candidate it shows.
type HarnessChange string

// The changes.
const (
	// HarnessChangeSave saves the candidate as the harness's shared settings
	// and logins.
	HarnessChangeSave HarnessChange = "save"
	// HarnessChangeDiscard drops it, leaving the box's own files as they are.
	HarnessChangeDiscard HarnessChange = "discard"
)

// HarnessChangesSaving is what the review says saving and discarding do.
const HarnessChangesSaving = "Save updates shared harness defaults and credentials. Discard leaves local files unchanged."

// HarnessChangesNothing is what the review says of a candidate that carries
// nothing to save.
const HarnessChangesNothing = "No saveable settings or credentials."

// Label is how the review heads a changed file: what the change does to it,
// and, under a person's own file, that boxes go on being given theirs.
func (f HarnessChangedFile) Label() string {
	label := f.Action + " " + f.Path
	if f.Shadowed {
		label += " — shadowed by your override, which boxes keep getting"
	}
	return label
}

// Summary is the review's first line: where the candidate stands.
func (c HarnessChanges) Summary() string {
	return fmt.Sprintf("State: %s · revision %d", c.State, c.Revision)
}

// Empty is whether the candidate carries nothing to save.
func (c HarnessChanges) Empty() bool { return len(c.Files)+len(c.Credentials) == 0 }

// HarnessChangeOffer is one thing the review offers to do with what it
// shows, and why it cannot when it cannot.
type HarnessChangeOffer struct {
	Change  HarnessChange
	Label   string
	Enabled bool
	Why     string
}

// Offers is what the review offers for the candidate it shows: saving a
// pending one that carries something, or one whose save is still being
// handed to the harness; discarding a pending one.
func (c HarnessChanges) Offers() []HarnessChangeOffer {
	saveable := c.CandidateID != "" && ((c.State == "pending" && !c.Empty()) || c.State == "saved-handoff-pending")
	discardable := c.CandidateID != "" && c.State == "pending"
	offer := func(change HarnessChange, label string, enabled bool, why string) HarnessChangeOffer {
		if enabled {
			why = ""
		}
		return HarnessChangeOffer{Change: change, Label: label, Enabled: enabled, Why: why}
	}
	return []HarnessChangeOffer{
		offer(HarnessChangeSave, "Save reviewed changes", saveable, "no saveable candidate"),
		offer(HarnessChangeDiscard, "Discard candidate", discardable, "no pending candidate"),
	}
}

// RunHarnessChange saves or discards the candidate a review showed, by the
// ID it showed. It reads the box's changes again first, and refuses as the
// review would: a candidate that is no longer the box's (it captured a newer
// one, or the shown one was answered), or an offer that does not apply, in its
// words. It is the state the candidate is left in.
func RunHarnessChange(ctx context.Context, src DataSource, boxID string, change HarnessChange, candidateID string) (string, error) {
	if candidateID == "" {
		return "", errors.New("no candidate was reviewed")
	}
	current, err := src.HarnessChanges(ctx, boxID)
	if err != nil {
		return "", err
	}
	if current.CandidateID != candidateID {
		return "", errors.New("the box's harness changes moved since they were reviewed; review them again")
	}
	for _, o := range current.Offers() {
		if o.Change != change {
			continue
		}
		if !o.Enabled {
			return "", errors.New(o.Why)
		}
		if change == HarnessChangeSave {
			return src.SaveHarnessChanges(ctx, boxID, candidateID)
		}
		return src.DiscardHarnessChanges(ctx, boxID, candidateID)
	}
	return "", fmt.Errorf("no harness change %q", change)
}

// The explainer (ADR 26-11-02-152): the first time a project's console sees a
// discobox with a harness change to save, it says what saving one does before
// anybody is asked to. The review says what changed; this says what Save
// means — that the change stops being this box's and becomes every box's on
// the harness, a login included — which is the one thing about it a person
// cannot see from the diff.
//
// Once is per project, and the project is what remembers
// (Session.ExplainHarnessChanges): a file on this machine would explain again
// on a second machine and never to the second person on the project. A
// console marks the project once the explainer is answered, whichever way,
// leaving it included: explaining twice because somebody pressed Not now
// would be the explainer outstaying its one job (ExplainedHarnessChanges).
// Only a window that goes with it still up leaves it owed. It comes up
// only on a free screen (ExplainerScreen.Free): what is on the screen is each
// renderer's to report, and whether that leaves it free is the core's.

// The explainer's answers.
const (
	// ExplainReview opens the box's harness changes for review.
	ExplainReview = "review"
	// ExplainLater leaves them, for the workspace's notice to raise again.
	ExplainLater = "later"
)

// HarnessChangesToExplain is the box the explainer is about, when the window
// owes it: the box on screen (workspace, nil for none) when it has a change
// to save, else the first listed box that does. None when nothing listed has
// one.
func HarnessChangesToExplain(workspace *Sandbox, listed []Sandbox) (Sandbox, bool) {
	if workspace != nil && workspace.HarnessConfigPending {
		return *workspace, true
	}
	for _, box := range listed {
		if box.HarnessConfigPending {
			return box, true
		}
	}
	return Sandbox{}, false
}

// HarnessChangesExplainer is the explainer for box: what saving its harness
// changes does, asked as a pick between reviewing them and leaving them.
// Leaving it unanswered is Not now.
func HarnessChangesExplainer(box Sandbox) Question {
	name := HarnessOf(box)
	return Question{
		Title:   "Saving harness changes",
		Subject: box.Title(),
		Body: name + " in this discobox changed its settings or signed in.\n\n" +
			"Saving makes those settings, and that login, the shared default for every discobox on " + name + " in this project: " +
			"new discoboxes start with them, and a saved login is the one all of them use.\n\n" +
			"Settings can include hooks and commands, which then run in every discobox on " + name + ": read what changed before you save it.\n\n" +
			"Nothing is shared until you save. Discarding the change, or leaving it, keeps it in this discobox only.\n\n" +
			"Whenever a discobox has a change to save, its workspace says so above its terminal, and pressing that reviews it.",
		Choices: []Choice{
			{Key: ExplainReview, Label: "Review changes", Hint: "see what changed, then save or discard it"},
			{Key: ExplainLater, Label: "Not now", Hint: "the workspace goes on saying so, to review it later"},
		},
	}
}

// PromptQuiet is how long the composer has to have gone untyped in before the
// explainer may come up over it: a person typing a prompt would answer the
// explainer with the next key they press.
const PromptQuiet = 5 * time.Second

// ExplainerScreen is what a console reports of its screen, for whether the
// explainer may come up on it now.
type ExplainerScreen struct {
	// Covered is something in front of the window holding the keys: a
	// dialog, a sheet, a menu, the palette, the run options.
	Covered bool
	// Terminal is a box's terminal with the keys — or about to have them, its
	// workspace opened on its panes. The change is made by signing in there,
	// so the person may still be typing in it when it lands.
	Terminal bool
	// Composing is the composer with the keys, Draft whether it holds a
	// prompt, and SinceTyped how long since a key last went into it.
	Composing  bool
	Draft      bool
	SinceTyped time.Duration
}

// Free is whether nothing else has the screen: nothing in front of the
// window, no terminal with the keys, and no prompt being written — the
// composer, if it has the keys, empty and untyped in for PromptQuiet.
func (s ExplainerScreen) Free() bool {
	writing := s.Composing && (s.Draft || s.SinceTyped < PromptQuiet)
	return !s.Covered && !s.Terminal && !writing
}

// ExplainedHarnessChanges records on the project that the explainer was
// answered, whichever way: what it returns is what a console says when the
// record did not land, empty when it did. The explainer has been read either
// way, and losing the record costs one repeated explanation.
func ExplainedHarnessChanges(ctx context.Context, src DataSource) string {
	if err := src.MarkHarnessChangesExplained(ctx); err != nil {
		return "could not save that harness changes were explained: " + err.Error()
	}
	return ""
}
