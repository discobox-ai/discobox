package console

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Creating a discobox (ADR 26-11-07-876 §1): what the create card offers, the
// questions a create asks before it is sent, and the run options as forms for
// a renderer that draws the core's forms. The TUI draws the card and the panel
// itself over RunOptions; the desktop app fills CreateForm and OptionsForm.
// Either way the rows, their words and the request they make are these.

// LeavesWindow reports whether a prompt is a harness's own way out, "/exit"
// or "/quit": somebody who types one into the composer means to leave the
// window too, not to cut a discobox and hand it a slash command.
func LeavesWindow(prompt string) bool {
	v := strings.TrimSpace(prompt)
	return v == "/exit" || v == "/quit"
}

// CardRows are the settings the create card offers under where it is cut
// from, as indexes into the rows: the harness first, since it is the one most
// often changed, then the pool and the server where there is one to choose,
// then what is brought in beside the source. The rest are the run options'
// alone (OptionsForm, the TUI's panel).
func (o *RunOptions) CardRows() []int {
	rows := []int{OptHarness}
	// The pool where there is more than one to choose, by the panel's own
	// rule: a row with nothing to step to is a row that only says what the
	// summary already does.
	if o.Shown(OptPool) {
		rows = append(rows, OptPool)
	}
	if o.Server() != nil {
		rows = append(rows, OptServer)
	}
	return append(rows, OptProjectSkills, OptUserSkills, OptSkills, OptInclude)
}

// CardChoice is how one of a row's choices reads on the card, which is the
// choice itself except for a harness nobody chose: "auto", and what that is
// where the project says. The panel names the default outright; the card is
// read at the moment of creating, where what matters is that nobody has to
// pick.
func (o *RunOptions) CardChoice(opt *RunOption, i int) string {
	if opt != o.Opts[OptHarness] || i != 0 || len(opt.Choices) == 0 {
		if i < 0 || i >= len(opt.Choices) {
			return ""
		}
		return opt.Choices[i]
	}
	if opt.Choices[0] == UnsetHarness {
		return "auto"
	}
	return "auto (" + opt.Choices[0] + ")"
}

// CardValue is how a row reads on the card (CardChoice for a choice).
func (o *RunOptions) CardValue(opt *RunOption) string {
	if opt.Kind != OptionChoice {
		return opt.Display()
	}
	return o.CardChoice(opt, opt.Index)
}

// CardEnterHint is the card's hint on the row that asks for a source of its
// own (SourceHint on a source).
const CardEnterHint = "SOURCE · a path, URL or owner/repo, with @REF and //PATH, the listing has never seen"

// CardRequest is the create a card makes from one source: the run options as
// they stand, and — for a card that is `discobox run`'s own question (base) —
// what the card has no row for taken from that run as given: the prompt's own
// tokens, --bind, --delegate, --declared-sources.
func (o *RunOptions) CardRequest(prompt, source string, base *RunRequest) RunRequest {
	req := o.Request(prompt, source)
	if base != nil {
		req.Prompt = append([]string(nil), base.Prompt...)
		req.Bind, req.Delegate, req.DiscoboxAPI = base.Bind, base.Delegate, base.DiscoboxAPI
		req.SkipDeclaredSources = base.SkipDeclaredSources
	}
	return req
}

// ErrNoHarness is a create in a project with no harness to run at all.
var ErrNoHarness = errors.New("this project has no harness to run; register one with `discobox admin harness create`")

// DefaultHarnessQuestion is asked of a create that names no harness in a
// project with no default (ADR 0048): the server refuses that create
// outright, so the alternative to asking is the same refusal a moment later
// with the answer left to the user to find. It reports whether there is
// anything to ask: not when the request names a harness or the project has a
// default, and not before the harnesses are listed (listed false), when the
// server's refusal is the answer this would only be guessing at. A project
// with nothing to offer is ErrNoHarness.
//
// `shell` is not among the choices. It runs like any other harness and is
// chosen like any other, but a project whose default is a login shell has no
// coding harness by default, which is the state this is trying to leave.
// Each choice's key is its place among Candidates; ChosenHarness reads one.
func DefaultHarnessQuestion(req RunRequest, harnesses []Harness, listed bool) (Question, bool, error) {
	if req.Harness != "" || !listed || slices.ContainsFunc(harnesses, func(h Harness) bool { return h.Default }) {
		return Question{}, false, nil
	}
	candidates := HarnessCandidates(harnesses)
	if len(candidates) == 0 {
		return Question{}, false, ErrNoHarness
	}
	q := Question{
		Title: "Which harness should this project run?",
		Body:  "Nothing is set as the project default, so a discobox has no harness to start.",
	}
	for i, harness := range candidates {
		q.Choices = append(q.Choices, Choice{Key: strconv.Itoa(i + 1), Label: harness.DisplayName(), Hint: "make it the project default"})
	}
	return q, true, nil
}

// HarnessCandidates are the harnesses worth offering as a project default: all
// of them except `shell`, in the order the listing reports.
func HarnessCandidates(harnesses []Harness) []Harness {
	var out []Harness
	for _, harness := range harnesses {
		if !harness.Shell {
			out = append(out, harness)
		}
	}
	return out
}

// ChosenHarness is the harness DefaultHarnessQuestion's answer names.
func ChosenHarness(harnesses []Harness, key string) (Harness, bool) {
	candidates := HarnessCandidates(harnesses)
	i, err := strconv.Atoi(key)
	if err != nil || i < 1 || i > len(candidates) {
		return Harness{}, false
	}
	return candidates[i-1], true
}

// WorkspaceChecked reports whether a create needs its working tree read
// (DataSource.Workspace) before it is sent: not for a discobox with no source,
// which has no working tree to carry anything out of, and not once both
// questions about it are answered.
func WorkspaceChecked(req RunRequest) bool {
	return !req.NoSource && (req.IncludeDirty == "" || req.CopyDirectory == "")
}

// WorkspaceQuestion is what a create asks about what local content reaches
// the discobox, once its working tree has been read. Both answers are
// answers — the discobox is created either way.
//
// The two questions differ in what they decide and which answer leads. About
// a repository's uncommitted work, excluding leads, so the quick answer means
// the discobox sees the committed history — the default answer is the one
// that changes nothing about what the sandbox sees; the answer is
// IncludeDirty. About a directory in no repository there is no commit to start
// from instead, so copying leads; the answer is CopyDirectory, and answering
// no creates the discobox with nothing checked out in it, the way --no-source
// does (ADR 0077 §1). Subject is the directory.
type WorkspaceQuestion struct {
	Question
	// Repository is whether it is the repository's question.
	Repository bool
}

// AskAboutWorkspace is the question a read working tree asks, and whether it
// asks one: only when the tree has something in it that a request has not
// already answered for.
func AskAboutWorkspace(req RunRequest, ws SourceWorkspace) (WorkspaceQuestion, bool) {
	if !ws.Carries {
		return WorkspaceQuestion{}, false
	}
	if !ws.Repository {
		if req.CopyDirectory != "" {
			return WorkspaceQuestion{}, false
		}
		return WorkspaceQuestion{Question: Question{
			Title:   "Copy this directory?",
			Subject: ws.Directory,
			Body:    CopyDirectoryBody,
			Choices: []Choice{
				{Key: "true", Label: "Copy the directory in", Hint: "Everything in it arrives as uncommitted changes"},
				{Key: "false", Label: "Do not copy the directory", Hint: "Create the discobox with nothing checked out in it"},
			},
		}}, true
	}
	if req.IncludeDirty != "" {
		return WorkspaceQuestion{}, false
	}
	return WorkspaceQuestion{Repository: true, Question: Question{
		Title:   "Uncommitted changes",
		Subject: ws.Directory,
		Body:    dirtyWorkspaceBody(ws),
		Choices: []Choice{
			{Key: "false", Label: "Start from the last commit", Hint: "Leave the uncommitted changes here"},
			{Key: "true", Label: "Include uncommitted changes", Hint: "Start the discobox from a snapshot of the working tree"},
		},
	}}, true
}

// Answer is the request with the question answered by key.
func (q WorkspaceQuestion) Answer(req RunRequest, key string) RunRequest {
	if q.Repository {
		req.IncludeDirty = key
	} else {
		req.CopyDirectory = key
	}
	return req
}

// Dismissed is what leaving the question answers: the repository's keeps its
// quick answer, starting from the last commit, and leaving the copy question
// creates nothing (ok false).
func (q WorkspaceQuestion) Dismissed() (key string, ok bool) {
	if q.Repository {
		return "false", true
	}
	return "", false
}

// CopyDirectoryBody is the copy question, under the directory it names. What
// it would cost is not in here: a renderer that counts it as the question is
// up (DataSource.MeasureDirectory) says it beside, since it is the whole of
// what the answer turns on and it is still arriving.
const CopyDirectoryBody = "It is not a Git repository, so copying it into the discobox means copying all of it. " +
	"Answering no creates the discobox anyway, with nothing checked out in it."

// dirtyWorkspaceBody says what the question is about under the directory it
// names: how many paths differ from the checked-out commit, and enough of them
// to recognize the change by. A listing that reported no paths still asks the
// question — what it is about is the working tree, not the count.
func dirtyWorkspaceBody(workspace SourceWorkspace) string {
	changes := "It has uncommitted changes"
	if n := len(workspace.Changes); n > 0 {
		changes = fmt.Sprintf("It has %s (%s)",
			count(n, "uncommitted change", "uncommitted changes"), summarizePaths(workspace.Changes))
	}
	return changes + ". Carry them into the discobox as a snapshot on top of the checked-out commit?"
}

// summarizePaths is a few of the paths and then how many more there are: enough
// to recognize the change by, without turning the question into a file listing.
func summarizePaths(paths []string) string {
	const shown = 3
	summary := strings.Join(paths[:min(shown, len(paths))], ", ")
	if len(paths) > shown {
		summary = fmt.Sprintf("%s and %d more", summary, len(paths)-shown)
	}
	return summary
}

// ---------------------------------------------------------------------------
// The run options as forms

// The keys of the run options' fields: a choice row's and a toggle's by the
// row, a repeated row's one field per value (OptionItemKey).
const (
	// SourceField is where a create is cut from: a source's value,
	// SourceNone, or SourceEnter for one typed into TypedSourceKey.
	SourceField = "source"
	// TypedSourceKey is the field a source the listing has never seen, typed.
	TypedSourceKey = "source:typed"
	// SourceEnter is the source field's choice that asks for one typed.
	SourceEnter = "\x00enter"
)

var optionKeys = map[int]string{
	OptHarness: "harness", OptDirty: "dirty", OptDetach: "detach", OptEnv: "env", OptSecret: "secret",
	OptProjectSkills: "projectSkills", OptUserSkills: "userSkills", OptSkills: "skills", OptInclude: "include",
	OptPool: "pool", OptServer: "server",
}

// OptionItemKey is the key of a repeated row's i-th value.
func OptionItemKey(opt, i int) string { return optionKeys[opt] + ":" + strconv.Itoa(i) }

// valueAt is what choice i stands for (Selected, for any choice).
func (o *RunOption) valueAt(i int) string {
	if o.Values != nil {
		return o.Values[i]
	}
	return o.Choices[i]
}

// RunAnswer is what a run options form was answered with: every row it asked,
// by what each choice stands for, never its place: the rows can be rebuilt
// between the form and its answer (another listing, another server), and a
// place would then name another choice. TakeOptions and TakeCard set the rows
// to it.
type RunAnswer struct {
	// Chosen are the choice rows' values (RunOption.Selected) and the
	// toggles' "on" or "off", by row.
	Chosen map[int]string
	// Items are the repeated rows' values, by row.
	Items map[int][]string
	// Source is where a create is cut from, or the Source row is set to: a
	// source's value, or SourceNone. Named is set when it was typed and
	// checked (DataSource.ResolveSource).
	Source string
	Named  *Source
}

// apply sets the rows to an answer, each choice found again by its value; a
// value the row no longer offers leaves the row as it is.
func (o *RunOptions) apply(a RunAnswer) {
	for row, value := range a.Chosen {
		opt := o.Opts[row]
		if opt.Kind == OptionToggle {
			opt.Value = value
			continue
		}
		for i := range opt.Choices {
			if opt.valueAt(i) == value {
				opt.Index = i
				break
			}
		}
	}
	for row, items := range a.Items {
		o.Opts[row].Items = append([]string(nil), items...)
	}
}

// TakeOptions sets the rows to the run options' answer, the Source row
// included: where the next create is cut from.
func (o *RunOptions) TakeOptions(a RunAnswer) {
	o.apply(a)
	o.takeSource(a)
}

// TakeCard sets the rows to the create card's answer and cuts from its source
// (CutFrom): where this box is cut from is where the next create opens.
func (o *RunOptions) TakeCard(a RunAnswer) {
	o.apply(a)
	if a.Named != nil {
		o.Named = *a.Named
	}
	o.CutFrom(a.Source)
}

func (o *RunOptions) takeSource(a RunAnswer) {
	if a.Named != nil {
		o.NameSource(*a.Named)
		return
	}
	o.ChooseSource(a.Source)
}

// Resolve checks a source typed by hand (DataSource.ResolveSource).
type Resolve func(source string) (Source, error)

// CreateForm is the create card as a form: where the discobox is cut from —
// every source the Source row offers, or one typed — and the card's settings
// (CardRows). Its Note is the command the answers would run. Each source typed,
// and each other source added, is checked (resolve) before the form takes it,
// and its refusal is said against the field it was typed in.
func (o *RunOptions) CreateForm(prompt string, resolve Resolve) Form[RunAnswer] {
	return o.runForm(o.CardRows(), true, prompt, resolve)
}

// OptionsForm is the run options as a form: where creates are cut from (the
// Source row, as the card asks it) and every other row there is to choose
// (Shown), with the command they make as its Note.
func (o *RunOptions) OptionsForm(prompt string, resolve Resolve) Form[RunAnswer] {
	var rows []int
	for i := range o.Opts {
		if i != OptSource && o.Shown(i) {
			rows = append(rows, i)
		}
	}
	return o.runForm(rows, false, prompt, resolve)
}

func (o *RunOptions) runForm(rows []int, card bool, prompt string, resolve Resolve) Form[RunAnswer] {
	section := "Options"
	sourceRow := o.Opts[OptSource]
	source := Field{Key: SourceField, Label: "cut from", Section: "Source", Hint: SourceHint, Value: sourceRow.Selected()}
	for i, value := range sourceRow.Values {
		source.Choices = append(source.Choices, Choice{Key: value, Label: sourceRow.Choices[i], Hint: o.SourceDetail(value)})
	}
	source.Choices = append(source.Choices, Choice{Key: SourceEnter, Label: "another…", Hint: CardEnterHint})
	typed := Field{
		Key: TypedSourceKey, Label: "source", Section: "Source", Hint: CardEnterHint,
		Placeholder: o.SourceLabel(), Value: o.TypedSource(),
		When: func(a Answers) bool { return a[SourceField] == SourceEnter },
		Why:  "choose another… to type one",
	}
	fixed := []Field{source, typed}
	for _, row := range rows {
		opt := o.Opts[row]
		switch opt.Kind {
		case OptionChoice:
			f := Field{Key: optionKeys[row], Label: strings.ToLower(opt.Label), Section: section, Hint: opt.Hint, Value: opt.Selected()}
			for i := range opt.Choices {
				label := opt.Choices[i]
				if card {
					label = o.CardChoice(opt, i)
				}
				f.Choices = append(f.Choices, Choice{Key: opt.valueAt(i), Label: label})
			}
			fixed = append(fixed, f)
		case OptionToggle:
			fixed = append(fixed, Field{Key: optionKeys[row], Label: strings.ToLower(opt.Label), Section: section, Hint: opt.Hint, Value: opt.Value,
				Choices: []Choice{{Key: "off", Label: "no"}, {Key: "on", Label: "yes"}}})
		}
	}
	// The repeated rows go last, each growing a field as its last is filled.
	var lists []int
	for _, row := range rows {
		if o.Opts[row].Kind == OptionList {
			lists = append(lists, row)
		}
	}
	grow := func(a Answers) []Field {
		fields := append([]Field(nil), fixed...)
		for _, row := range lists {
			opt := o.Opts[row]
			n := len(opt.Items) + 1
			for i := 0; ; i++ {
				v, ok := a[OptionItemKey(row, i)]
				if !ok {
					break
				}
				if strings.TrimSpace(v) != "" {
					n = max(n, i+2)
				}
			}
			for i := range n {
				f := Field{Key: OptionItemKey(row, i), Label: strings.ToLower(opt.Label), Section: opt.Label, Hint: opt.Hint, Placeholder: opt.Example}
				if i > 0 {
					f.Label = "and"
					f.Placeholder = "another (optional)"
				}
				if i < len(opt.Items) {
					f.Value = opt.Items[i]
				}
				fields = append(fields, f)
			}
		}
		return fields
	}
	// read is the answers as the rows take them, each source typed checked
	// by resolve; with none, it is taken as typed.
	read := func(a Answers, resolve Resolve) (RunAnswer, *Refusal) {
		out := RunAnswer{Chosen: map[int]string{}, Items: map[int][]string{}}
		for _, row := range rows {
			opt := o.Opts[row]
			if opt.Kind == OptionList {
				var items []string
				for i := 0; ; i++ {
					v, ok := a[OptionItemKey(row, i)]
					if !ok {
						break
					}
					if v = strings.TrimSpace(v); v == "" {
						continue
					}
					if opt.Checked && !slices.Contains(opt.Items, v) && resolve != nil {
						if _, err := resolve(v); err != nil {
							return RunAnswer{}, &Refusal{Field: OptionItemKey(row, i), Why: err.Error()}
						}
					}
					items = append(items, v)
				}
				out.Items[row] = items
				continue
			}
			chosen, ok := a[optionKeys[row]]
			if !ok {
				continue
			}
			out.Chosen[row] = chosen
		}
		switch chosen := a[SourceField]; chosen {
		case SourceEnter:
			typed := a.Get(TypedSourceKey)
			if typed == "" {
				// Nothing typed is the folder the field offered as its
				// placeholder.
				out.Source = o.SourceDir()
				break
			}
			if resolve == nil {
				out.Source = typed
				break
			}
			resolved, err := resolve(typed)
			if err != nil {
				return RunAnswer{}, &Refusal{Field: TypedSourceKey, Why: err.Error()}
			}
			out.Source, out.Named = resolved.Value, &resolved
		default:
			out.Source = chosen
		}
		return out, nil
	}
	// The note is what the answers would run, read without checking what
	// was typed: a draft is sent as each key is, and checking is the submit's.
	note := func(a Answers) string {
		ans, refusal := read(a, nil)
		if refusal != nil {
			return ""
		}
		preview := o.Clone()
		preview.apply(ans)
		return preview.CommandFor(preview.Request(prompt, ans.Source))
	}
	return Form[RunAnswer]{
		Fields: grow(nil), Grow: grow, Note: note,
		Submit: func(a Answers) (RunAnswer, *Refusal) { return read(a, resolve) },
	}
}
