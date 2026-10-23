package tui

import (
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/discobox-ai/discobox/pkg/sourceref"
)

// runOptions is every flag `discobox run` takes, laid out as one editable list.
//
// The panel is a picker, not a form: left and right change a value in place,
// so the common case — swap the harness, turn dirty-carry off, cut from the
// repository next door — never opens a text field. Only the repeated flags
// (env, secrets) and a source the listing has never seen do.
type optKind int

const (
	optChoice optKind = iota
	optToggle
	optMulti // repeated flag: -e, -s
)

type option struct {
	label   string
	kind    optKind
	choices []string
	// values are what the choices stand for, when what a choice is called is
	// not what the command takes. Nil leaves every choice its own value, which
	// is every row but the source.
	values []string
	// unchanged is the value this row counts as the CLI's own default, for a
	// row whose choices are not in a fixed order with the default leading.
	// Empty leaves the default at index zero, which is every row but the
	// source — whose default is wherever the header has moved to.
	unchanged string
	idx       int
	value     string
	items     []string
	hint      string
	// example is what the field a repeated flag's value is typed into shows
	// while it is empty.
	example string
	// checked marks a repeated flag whose values name sources, which are
	// checked the way a source typed by hand is before the row takes them.
	// See Model.askForItem.
	checked bool
}

// changed reports whether the option differs from the CLI's own default, which
// is what decides if it shows up in the chip strip and in the command preview.
func (o *option) changed() bool {
	switch o.kind {
	case optChoice:
		if o.unchanged != "" {
			return o.selected() != o.unchanged
		}
		return o.idx != 0
	case optToggle:
		return o.value == "on"
	case optMulti:
		return len(o.items) > 0
	}
	return false
}

// selected is the value the current choice stands for: the choice itself
// unless the row carries values of its own.
func (o *option) selected() string {
	if o.idx < 0 || o.idx >= len(o.choices) {
		return ""
	}
	if o.values != nil {
		return o.values[o.idx]
	}
	return o.choices[o.idx]
}

func (o *option) display() string {
	switch o.kind {
	case optChoice:
		if len(o.choices) == 0 {
			return ""
		}
		return o.choices[o.idx]
	case optToggle:
		if o.value == "on" {
			return "yes"
		}
		return "no"
	case optMulti:
		if len(o.items) == 0 {
			return "(none)"
		}
		return strings.Join(o.items, "  ")
	}
	return ""
}

func (o *option) cycle(delta int) {
	switch o.kind {
	case optChoice:
		if len(o.choices) == 0 {
			return
		}
		o.idx = (o.idx + delta + len(o.choices)) % len(o.choices)
	case optToggle:
		if o.value == "on" {
			o.value = "off"
		} else {
			o.value = "on"
		}
	}
}

// optionSet is the panel: the options plus its own cursor. The project and the
// directory come from the session rather than the panel, and are carried here
// only so the command preview can show the flags it would need.
type optionSet struct {
	opts    []*option
	cursor  int
	session Session

	// folder is the source of the folder the window is currently working in,
	// chosen in the header: the directory or URL a create from it cuts from. It
	// is where Enter creates from, so switching folders switches what a new
	// sandbox is cut from as well as which ones are listed — the header is one
	// control, not two that happen to look alike. Empty is the session's own
	// directory, which is also what "all folders" falls back to: there is no
	// single folder to create in then.
	folder string

	// known are the sources the project's discoboxes were cut from, newest
	// first, as the listing reports them. They are what the Source row offers
	// besides the folder itself: the places this project is worked in are
	// exactly the ones already holding something.
	known []Source
	// source is what the Source row is set to: a directory, a repository URL,
	// sourceNone, or empty for the folder the header is on. It is kept here
	// rather than read back off the row's cursor, because the row is rebuilt
	// whenever the folder or the listing moves under it.
	source string
	// named is the last source typed by hand, as the check of it reported it.
	// The listing does not hold it, so where a discobox cut from it is filed
	// is kept here for the list to follow it there.
	named Source
	// lastCut is the source the last create card was answered with
	// (askHowToCreate), in this window or — through Session.LastSource — the
	// last one on this machine. It leads the Source row, and answering a card
	// sets the row to it, so creating again from the same place is one Enter.
	// Empty until a card has ever been answered.
	lastCut string
}

// Source is one place the project's discoboxes have been cut from, as the
// Source row offers it.
type Source struct {
	// Value is what `run`'s SOURCE takes: the client directory, or the
	// repository URL.
	Value string
	// Remote marks a repository URL rather than a directory on a client.
	Remote bool
	// Directory is the directory a local Value names, without its ref or
	// path, as the data source resolved it; empty for a remote. It is not
	// split off Value here: a directory's own name may hold `@` or `//`,
	// which only its disk tells from a ref or a path (sourceref.Ref.Resolve).
	Directory string
	// OriginKey is where a discobox cut from Value on this machine is filed
	// (ADR 0111), which is the folder the list follows the source to.
	OriginKey string
	// RepositoryKey is the GitHub repository Value belongs to, keyed as
	// Sandbox.RepositoryKey is, and empty when it is not on GitHub: the
	// folder the list follows the source to is then the repository's.
	RepositoryKey string
}

// sourceNone is what the Source row carries for a discobox with nothing
// checked out in it. It is a sentinel rather than the empty string because
// empty already means "the folder the header is on", and the two are different
// answers; the NUL keeps it from ever colliding with a path or a URL.
const sourceNone = "\x00no-source"

// noSourceChoice is how sourceNone reads, in the panel and on the chip strip
// both. It is the flag's own words rather than "(none)", which reads as a value
// that has not been filled in yet rather than as the answer it is.
const noSourceChoice = "no source"

// enterSourceChoice is the dropdown's last row: the one entry that is not a
// source but a way to name one the listing has never seen. It is only in the
// dropdown, never in the left-right cycle, because cycling onto it would mean
// stopping on a value that is not one.
const enterSourceChoice = "enter a directory, URL, or owner/repo, with @REF and //PATH…"

// Index into optionSet.opts. The command builder reads by name, so the order
// here is only the order they are shown in.
//
// These are `discobox run`'s flags and nothing else: --harness, --include-dirty,
// --detach, -e and -s, SOURCE, the source the sandbox is cut from, and what is
// brought in beside it — --user-skills, --skills and -i. A launcher that offers
// options the command does not have is a launcher you cannot reproduce from a
// shell.
//
// The project is not among them. It is set once for the session, the way
// DISCOBOX_PROJECT sets it for a shell, so it belongs in the header rather than
// in a panel you would have to open to find out where a sandbox went.
const (
	optHarness = iota
	optDirty
	optDetach
	optEnv
	optSecret
	optSource
	optProjectSkills
	optUserSkills
	optSkills
	optInclude
	// optServer is last because it is the one row that is not always there:
	// it exists only when there is a server to choose (ADR 0116 §5), and the
	// rows before it keep their places either way. See optionSet.server.
	optServer
)

// unsetHarness is what the harness row says when nothing has been chosen on it.
// It is spelled out rather than left blank, because a list with an empty row in
// it reads as a bug.
//
// There is no "none" among the choices. Running without a coding harness is the
// `shell` harness (ADR 0043), which is one of the project's like any other, so
// an entry meaning the same thing would be a second way to say it.
const unsetHarness = "(default)"

func newOptions(session Session) *optionSet {
	// The choices are the project's harnesses, which are loaded on their own
	// and arrive here through setHarnesses. Until they do the row is empty
	// rather than offering "none (shell)" as if it were the default: a panel
	// that names the wrong harness for a moment is worse than one that names
	// none.
	set := &optionSet{session: session, opts: []*option{
		optHarness: {
			label: "Harness", kind: optChoice,
			hint: harnessesHint,
		},
		optDirty: {
			label: "Uncommitted changes", kind: optChoice,
			choices: []string{"auto", "include", "exclude"},
			hint:    "--include-dirty · auto asks when the working tree is dirty",
		},
		optDetach: {
			label: "Detach", kind: optToggle, value: "off",
			hint: "-d · create the discobox and print it, without attaching",
		},
		optEnv: {
			label: "Env", kind: optMulti,
			hint:    "-e · KEY=VALUE, or KEY to pass through from this shell",
			example: "KEY=VALUE",
		},
		optSecret: {
			label: "Secrets", kind: optMulti,
			hint:    "-s · KEY=VALUE, or KEY=<sec_id> to reference an existing secret",
			example: "KEY=VALUE",
		},
		optSource: {
			label: "Source", kind: optChoice,
			hint: sourceHint,
		},
		optProjectSkills: {
			// On is what every create does unless told otherwise: the
			// project's skills repositories, copied in ahead of any --skills
			// (ADR 26-10-14-894 §2).
			label: "Project skills", kind: optChoice,
			choices: []string{"on", "off"},
			hint:    "--no-project-skills · the project's skills repositories (" + SkillsKeyName + "), added ahead of any --skills",
		},
		optUserSkills: {
			// Left alone it is client.yaml's new.userSkills, which is what
			// `discobox run` gives a create that names neither way.
			label: "User skills", kind: optChoice,
			choices: []string{userSkillsConfigured, "on", "off"},
			hint:    "--user-skills · install your own ~/.claude/skills and ~/.agents/skills in the discobox",
		},
		optSkills: {
			label: "Skills", kind: optMulti,
			hint:    "--skills · a directory of skills, each a subdirectory holding a SKILL.md, or a Git URL[@REF][//PATH]",
			example: "./team-skills",
		},
		optInclude: {
			label: "Additional sources", kind: optMulti,
			hint:    "-i · another directory, URL, or owner/repo, with @REF, brought into the same discobox",
			example: "../other-repo", checked: true,
		},
	}}
	if len(session.Servers) > 1 {
		// The primary leads, so the common case is index zero and emits no
		// flag, and it says it is the primary: the name alone does not say
		// which of them a run goes to when nothing is chosen.
		choices := append([]string{session.Servers[0] + " (primary)"}, session.Servers[1:]...)
		set.opts = append(set.opts, &option{
			label: "Server", kind: optChoice,
			choices: choices,
			values:  append([]string(nil), session.Servers...),
			hint:    serverHint,
		})
	}
	set.rebuildSources()
	return set
}

// serverHint is what the Server row says.
const serverHint = "--server · the server the discobox is created on"

// server is the Server row, or nil when there is only the primary to create on.
func (o *optionSet) server() *option {
	if len(o.opts) > optServer {
		return o.opts[optServer]
	}
	return nil
}

// setServer points a create at the server the header has moved to, for the same
// reason setFolder points the source at its folder: the header is where the
// window says which server it is working on, and a run option left behind it
// would create somewhere the window does not say.
//
// A name the row does not carry — and the empty name the header's "all servers"
// holds — leaves it on the primary, which is where a create with no --server at
// all goes (ADR 0116 §5): every server at once is not an answer to which one a
// new discobox belongs on.
func (o *optionSet) setServer(name string) {
	row := o.server()
	if row == nil {
		return
	}
	row.idx = 0
	for i, value := range row.values {
		if value == name {
			row.idx = i
			return
		}
	}
}

// prefill sets the rows to what a request already says, for the card
// `discobox run` asks its source on: a flag given on the command line is an
// answer, and the card opens holding it rather than asking it again. A harness
// the listing does not offer is added to the row rather than dropped, so the
// create meets the server's answer about it rather than a different harness.
func (o *optionSet) prefill(req RunRequest) {
	if req.Harness != "" {
		harness := o.opts[optHarness]
		i := slices.Index(harness.choices, req.Harness)
		if i < 0 {
			harness.choices = append(harness.choices, req.Harness)
			i = len(harness.choices) - 1
		}
		harness.idx = i
	}
	switch req.IncludeDirty {
	case "true":
		o.opts[optDirty].idx = 1
	case "false":
		o.opts[optDirty].idx = 2
	}
	if req.NoProjectSkills {
		o.opts[optProjectSkills].idx = 1
	}
	if req.UserSkills != nil {
		o.opts[optUserSkills].idx = 2
		if *req.UserSkills {
			o.opts[optUserSkills].idx = 1
		}
	}
	o.opts[optEnv].items = append([]string(nil), req.Env...)
	o.opts[optSecret].items = append([]string(nil), req.Secret...)
	o.opts[optSkills].items = append([]string(nil), req.SkillDirs...)
	o.opts[optInclude].items = append([]string(nil), req.Include...)
}

// sourceHint is what the Source row says: what it sets, and the one thing about
// it that left and right cannot do.
const sourceHint = "SOURCE · where the discobox is cut from · Enter opens the whole list, and a path of your own"

// optionsKey opens the run options panel and closes it. It is a window key
// rather than each screen's own — see updateKey — so the panel is one press
// away from wherever the window is, and one press from being put away again.
const optionsKey = "ctrl+o"

// harnessesHint is what the harness row says while nothing has been chosen on
// it.
const harnessesHint = "--harness · " + HarnessesKeyName + " enables, disables and picks the default harness"

// setHarnesses rebuilds the harness choices from the harnesses the window knows
// about. The project default leads, so the common case is index zero and emits
// no flag at all — the same shape every other option here has.
//
// The listing is the one source of what harnesses there are: enabling one on
// the harnesses screen puts it here without the window being reopened, and
// disabling the one that was chosen falls back to the default rather than
// leaving a name the run would be refused for. A harness no pool can run is
// not offered at all, for the same reason (ADR 0145 §1) — the project default
// included, since leaving the row unset runs the default and the create would
// be refused for its platform or its image kind. The hint says so, rather than
// that there is no default.
func (o *optionSet) setHarnesses(all []Harness) {
	harnesses := make([]Harness, 0, len(all))
	unhostedDefault := ""
	for _, harness := range all {
		switch {
		case !harness.Unhosted:
			harnesses = append(harnesses, harness)
		case harness.Default:
			unhostedDefault = harness.flagName()
		}
	}
	harness := o.opts[optHarness]
	chosen := ""
	if harness.idx > 0 && harness.idx < len(harness.choices) {
		chosen = harness.choices[harness.idx]
	}

	var def string
	for _, harness := range harnesses {
		if harness.Default {
			def = harness.flagName()
		}
	}
	choices := make([]string, 0, len(harnesses)+1)
	if def != "" {
		choices = append(choices, def)
	} else {
		// Nothing to lead with. Promoting whichever harness happens to be
		// registered first would put a name at index zero that the window then
		// treats as the default and emits no flag for — which is how the strip
		// came to announce a harness nobody had chosen and the project had not
		// named.
		choices = append(choices, unsetHarness)
	}
	for _, harness := range harnesses {
		if name := harness.flagName(); name != "" && name != def {
			choices = append(choices, name)
		}
	}

	harness.choices = choices
	harness.idx = 0
	for i, choice := range choices {
		if choice == chosen {
			harness.idx = i
		}
	}
	harness.hint = "--harness · no project default, so pick one · " + HarnessesKeyName + " manages them"
	if unhostedDefault != "" {
		harness.hint = "--harness · the project default, " + unhostedDefault + ", runs on no pool here, so pick one · " + HarnessesKeyName + " manages them"
	}
	if def != "" {
		harness.hint = "--harness · unset is the project default, which is " + def + " · " + HarnessesKeyName + " manages them"
	}
}

// setFolder points the run source at the folder the header has moved to. The
// header is where the folder is chosen, so a source chosen earlier gives way to
// it: leaving a stale override behind would mean the window says one thing and
// creates in another.
//
// "All folders" names no place to cut from, so there the row goes to the last
// create's source when there is one, rather than falling back silently to the
// directory the window happens to be running in.
func (o *optionSet) setFolder(folder string) {
	o.folder = folder
	o.source = o.sourceDir()
	if folder == "" && o.lastCut != "" {
		o.source = o.lastCut
	}
	o.rebuildSources()
}

// setSources takes the sources the project's discoboxes were cut from, off the
// same listing the header filter's folders are built from. What was chosen survives a
// refresh, including a path typed by hand that no discobox has been cut from
// yet.
func (o *optionSet) setSources(known []Source) {
	o.known = known
	o.rebuildSources()
}

// rebuildSources lays the Source row out: the source the last create was cut
// from, the directory this window is running in, then every other source the
// project has been cut from, then "no source".
//
// The order does not follow what is chosen; only a create moves it, by moving
// what leads it. Leading with the folder the header is on would reorder the row
// every time the list followed a source to its folder, and left-right would
// then only ever reach the first two entries. Which one counts as unchanged is carried by the row instead, so the
// panel can say "this is not where the header is" without the order saying it.
func (o *optionSet) rebuildSources() {
	opt := o.opts[optSource]
	var choices, values []string
	add := func(value string) {
		// "No source" is appended once at the end, where it belongs; it is
		// also what o.source holds when it is chosen, so it has to be kept
		// out of the sources themselves.
		if strings.TrimSpace(value) == "" || value == sourceNone {
			return
		}
		// One source is one choice, however each discobox spelled it.
		for _, seen := range values {
			if sameSource(seen, value) {
				return
			}
		}
		choices = append(choices, o.sourceChoiceLabel(value))
		values = append(values, value)
	}
	// The last create's answer leads, and is added whether or not the listing
	// holds it: it can be a path typed by hand that nothing has been cut from
	// since, or one whose discoboxes have all gone, and it is still where the
	// next create opens.
	add(o.lastCut)
	add(o.session.Directory)
	for _, source := range o.known {
		add(source.Value)
	}
	// The folder the header is on and a source named by hand are things the
	// listing cannot be relied on to hold either: a folder whose discoboxes
	// were all cut from somewhere else is still a folder you can cut from, and
	// a path typed into the field has to survive the next refresh of the
	// listing.
	add(o.sourceDir())
	add(o.source)
	choices = append(choices, noSourceChoice)
	values = append(values, sourceNone)

	opt.choices, opt.values, opt.unchanged = choices, values, o.sourceDir()
	opt.idx = 0
	for i, value := range values {
		if sameSource(value, o.source) {
			opt.idx = i
			return
		}
	}
}

// sourceChoiceLabel is how one source reads on the row. The directory this
// window is running in wears its branch, the way the header spells it; nothing
// else has a branch that means anything here.
func (o *optionSet) sourceChoiceLabel(value string) string {
	if sameSource(value, o.session.Directory) {
		return o.session.sourceLabel()
	}
	return sourceref.Display(value, o.session.Home)
}

// cutFrom records the source a create was cut from and sets the row to it, so
// the next create opens there whatever the header is on. The header is not
// moved: the answer is where a discobox comes from, not a filter.
func (o *optionSet) cutFrom(source string) {
	o.lastCut = source
	o.chooseSource(source)
}

// openOnLastCut sets the row to where the last window's last create was cut
// from (Session.LastSource).
func (o *optionSet) openOnLastCut(source string) {
	if source == "" {
		o.lastCut = source
		o.rebuildSources()
		return
	}
	o.cutFrom(source)
}

// cycleSource moves the Source row and records what it landed on, since the row
// is rebuilt from that rather than from where its cursor happens to sit.
func (o *optionSet) cycleSource(delta int) {
	opt := o.opts[optSource]
	opt.cycle(delta)
	o.chooseSource(opt.selected())
}

// chooseSource sets the row to a source, from the cycle, from the dropdown, or
// from the input field. Nothing entered is the folder the header is on, which
// is what the field was offering as its placeholder.
func (o *optionSet) chooseSource(value string) {
	if strings.TrimSpace(value) == "" {
		value = o.sourceDir()
	}
	o.source = value
	o.rebuildSources()
}

// sourceFolder is where a discobox cut from the chosen source is filed (ADR
// 0111), and so which folder the list follows the source to. The window's own
// source files under the window's own folder, and so does "no source", since
// this machine's discoboxes with no source are in every one of its folders. A
// directory or a URL files under its own, as the listing — or the check of a
// source typed by hand — reported it, and is in its GitHub repository's folder
// when it resolved to one. A source nothing has reported on is not followed
// anywhere.
func (o *optionSet) sourceFolder() (folder, bool) {
	value := o.opts[optSource].selected()
	if value == sourceNone || sameSource(value, o.session.Directory) {
		return o.session.folder(), true
	}
	of := func(source Source) (folder, bool) {
		if !sameSource(source.Value, value) || source.OriginKey == "" {
			return folder{}, false
		}
		// A directory's folder is the directory, whatever ref was named with
		// it: the ref is the source row's to say, not the folder's.
		dir := value
		if !source.Remote && source.Directory != "" {
			dir = source.Directory
		}
		if source.RepositoryKey != "" {
			return folder{key: source.RepositoryKey, label: repositoryLabel(source.RepositoryKey), source: dir, local: true}, true
		}
		return folder{key: source.OriginKey, label: sourceref.Display(dir, o.session.Home), source: dir, local: true}, true
	}
	if f, ok := of(o.named); ok {
		return f, true
	}
	for _, source := range o.known {
		if f, ok := of(source); ok {
			return f, true
		}
	}
	return folder{}, false
}

// nameSource sets the row to a source typed by hand and checked, keeping what
// the check said about where a discobox cut from it is filed.
func (o *optionSet) nameSource(source Source) {
	o.named = source
	o.chooseSource(source.Value)
}

// remoteSource reports whether a value is a repository URL rather than a
// directory. The listing — or the check of a source typed by hand — says so
// per source rather than this package resolving the string, because whether
// a shorthand is a directory here is the creation path's to decide and not
// the window's.
func (o *optionSet) remoteSource(value string) bool {
	if sameSource(o.named.Value, value) {
		return o.named.Remote
	}
	for _, source := range o.known {
		if sameSource(source.Value, value) {
			return source.Remote
		}
	}
	return false
}

// followSource moves the panel's folder to where the chosen source files its
// discoboxes, and returns it for the list and the header to follow — or
// reports that nothing says where that is, and the header stays where it is.
// This is the source moving the header rather than the header moving the
// source, so the source itself is left exactly as it was chosen.
func (o *optionSet) followSource() (folder, bool) {
	f, ok := o.sourceFolder()
	if !ok {
		return folder{}, false
	}
	o.folder = f.source
	o.rebuildSources()
	return f, true
}

// typedSource is what the input field opens holding: a source named by hand
// rather than one the listing offered, so re-opening the field to fix a typo
// does not start from nothing. A choice made off the list leaves it empty and
// the field shows the folder as its placeholder.
func (o *optionSet) typedSource() string {
	if o.source == sourceNone || o.source == o.sourceDir() {
		return ""
	}
	for _, source := range o.known {
		if sameSource(source.Value, o.source) {
			return ""
		}
	}
	return o.source
}

// sourceChosenMsg carries the dropdown's answer back to the live model, for the
// same reason filterChosenMsg does: the dialog closed over the model by value.
// enter is the row that is not an answer but a request for the input field.
type sourceChosenMsg struct {
	source string
	enter  bool
	// typed says the answer came from the input field rather than off the
	// list, so it names a place nothing has vouched for and is checked before
	// it is taken. See Model.resolveSource.
	typed bool
	// card is the create card the answer came off, so answering it carries
	// that create on rather than only setting the row. Nil is the Source row's
	// own list. See Model.askHowToCreate.
	card *createCard
}

// sourceDialog is the Source row opened out: every source the project has been
// cut from, "no source", and the way to name one the listing has never seen.
// The create card's list is the same choices (createCard.view): the answer
// that settles the row is the answer that settles a create, and offering a
// different set depending on which of the two asked would be two controls
// wearing one name.
func (o *optionSet) sourceDialog() *dialog {
	opt := o.opts[optSource]
	items := make([]action, 0, len(opt.choices)+1)
	for i, choice := range opt.choices {
		n := itoa(i + 1)
		items = append(items, action{
			// The key is the row's index, so the first nine choices can be
			// picked by number as well as by moving to them.
			key:     n,
			press:   n,
			label:   choice,
			detail:  o.sourceDetail(opt.values[i]),
			enabled: true,
		})
	}
	items = append(items, action{key: "e", press: "e", label: enterSourceChoice, enabled: true})
	menu := actionsDialog("Cut the discobox from", "", items, func(key string) tea.Cmd {
		if key == "e" {
			return func() tea.Msg { return sourceChosenMsg{enter: true} }
		}
		for i, value := range opt.values {
			if itoa(i+1) == key {
				return func() tea.Msg { return sourceChosenMsg{source: value} }
			}
		}
		return nil
	})
	menu.cursor = opt.idx
	menu.keys = []hint{pressing("Enter cuts the next discobox from that", "enter"), pressing("Esc cancels", "esc")}
	return menu
}

// sourceDetail is what each choice is worth knowing beyond its own name.
func (o *optionSet) sourceDetail(value string) string {
	switch {
	case value == sourceNone:
		return "nothing checked out — an empty discobox"
	case value == o.sourceDir():
		if o.folder == "" {
			// Every folder at once is not a folder the window is showing, so
			// the directory it happens to be running in is described as what
			// it is — the same words the filter's card uses for it.
			return "where this window is running"
		}
		return "the folder this window is showing"
	case o.remoteSource(value):
		return "cloned by the discobox itself"
	default:
		return ""
	}
}

// sourceKey tells one thing to cut from from another however it is spelled
// — a repository's https URL with or without .git, in any case on GitHub, is
// one — so the row offers each once (sourceref.Ref.CloneKey, ADR 26-10-15-664
// §4). The transport is part of it: an ssh, http or git:// spelling of the
// same repository is fetched another way, with other credentials or none, so
// merging it would run a URL nobody chose.
func sourceKey(value string) string {
	source, err := sourceref.Parse(value)
	if value == sourceNone || err != nil {
		return value
	}
	return source.CloneKey()
}

// sameSource reports whether two values name one source.
func sameSource(a, b string) bool {
	return a == b || sourceKey(a) == sourceKey(b)
}

// sourceDir is what Enter creates from: the folder the header is on, or the
// window's own source when the header is on every folder at once.
func (o *optionSet) sourceDir() string {
	if o.folder == "" {
		return o.session.Directory
	}
	return o.folder
}

// sourceLabel is that directory as the header spells it, with the branch on it
// only where the branch means something — the directory this window is actually
// running in.
func (o *optionSet) sourceLabel() string {
	if sameSource(o.sourceDir(), o.session.Directory) {
		return o.session.sourceLabel()
	}
	return sourceref.Display(o.sourceDir(), o.session.Home)
}

// sourceLabel is what a sandbox is cut from by default, in the spelling the
// header uses: the window's directory and the branch checked out there.
func (s Session) sourceLabel() string {
	if s.Branch == "" {
		return sourceref.Display(s.Directory, s.Home)
	}
	return sourceref.Display(s.Directory, s.Home) + " @ " + s.Branch
}

func (o *optionSet) current() *option { return o.opts[o.cursor] }

func (o *optionSet) move(delta int) {
	o.cursor = (o.cursor + delta + len(o.opts)) % len(o.opts)
}

// moveTo puts the cursor on one row, which is what a press on it means. It
// clamps rather than wrapping: a pointer names a row that is there.
func (o *optionSet) moveTo(i int) {
	o.cursor = min(max(i, 0), len(o.opts)-1)
}

// request is what Enter actually asks for: the options as `discobox run`'s
// arguments, with the prompt from the composer. The source is passed rather
// than read off the row, because the answer to a create's own question is cut
// from without moving the row (askHowToCreate); everything else passes what
// the row has selected.
func (o *optionSet) request(prompt, source string) RunRequest {
	req := RunRequest{
		Detach: o.opts[optDetach].changed(),
		Env:    append([]string(nil), o.opts[optEnv].items...),
		Secret: append([]string(nil), o.opts[optSecret].items...),
	}
	// One argument, because the composer holds one piece of text and splitting
	// it would be inventing tokens the user did not type.
	if text := strings.TrimSpace(prompt); text != "" {
		req.Prompt = []string{text}
	}
	// The window's own directory is left empty: the create cuts from where the
	// window runs, and the preview writes it as ".".
	switch {
	case source == sourceNone:
		req.NoSource = true
	case source != o.session.Directory:
		req.Source = source
	}
	if h := o.opts[optHarness]; h.changed() {
		req.Harness = h.choices[h.idx]
	}
	switch o.opts[optDirty].idx {
	case 1:
		req.IncludeDirty = "true"
	case 2:
		req.IncludeDirty = "false"
	}
	req.NoProjectSkills = o.opts[optProjectSkills].changed()
	req.UserSkills = o.userSkills()
	req.SkillDirs = append([]string(nil), o.opts[optSkills].items...)
	req.Include = append([]string(nil), o.opts[optInclude].items...)
	if server := o.server(); server != nil && server.changed() {
		req.Server = server.selected()
	}
	return req
}

// chips is the mode line under the composer — Claude Code's "⏵⏵ bypass
// permissions on" slot. It carries every option, not only the changed ones,
// because the line exists to answer "what will Enter do" without opening
// anything. Env and secrets are counts, and a count of none is not worth a
// word.
func (o *optionSet) chips(st *styles) string {
	return o.renderChips(st, true)
}

// mutedChips is the same resolved run summary after focus has left the
// composer. Every glyph uses the muted style so the line recedes as one unit.
func (o *optionSet) mutedChips(st *styles) string {
	return o.renderChips(st, false)
}

func (o *optionSet) renderChips(st *styles, focused bool) string {
	parts := []string{}
	add := func(text string) {
		style := st.chipOn
		if !focused {
			style = st.chip
		}
		parts = append(parts, style.Render(text))
	}

	// The harness is always visible because it is the most consequential part
	// of what Enter will run. The resolved project default stays muted; an
	// explicit override uses gold like the other changed options.
	harness := o.opts[optHarness]
	if harness.display() != "" {
		style := st.chip
		if focused && harness.changed() {
			style = st.chipOn
		}
		parts = append(parts, style.Render(harness.display()))
	}

	// Only the answers that were given. Auto is what the option already does,
	// and a strip that names every default is a strip you stop reading for the
	// one thing on it that was chosen.
	switch o.opts[optDirty].idx {
	case 1:
		add("+dirty")
	case 2:
		add("clean")
	}

	// Attaching is what happens unless you say otherwise, and a line that
	// says what was always going to happen is a line you stop reading.
	if o.opts[optDetach].changed() {
		add("detached")
	}

	if n := len(o.opts[optEnv].items); n > 0 {
		add(plural(n, "env", "env"))
	}
	if n := len(o.opts[optSecret].items); n > 0 {
		add(plural(n, "secret", "secrets"))
	}

	// The source is only worth a chip when it is not where the window says it
	// is. The header already names the folder the window is working in, and a
	// strip that repeats it teaches you to stop reading the strip — which is
	// exactly what the row's leading choice is.
	if source := o.opts[optSource]; source.changed() {
		add(source.display())
	}

	// What is brought in beside the source, as counts for the same reason env
	// and secrets are: the strip says there is something, the panel says what.
	if o.opts[optProjectSkills].changed() {
		add("no project skills")
	}
	skills := len(o.opts[optSkills].items)
	user := o.userSkills()
	switch {
	case user != nil && *user && skills > 0:
		add("your skills +" + itoa(skills))
	case user != nil && *user:
		add("your skills")
	case user != nil:
		add("not your skills")
	}
	if skills > 0 && (user == nil || !*user) {
		add(plural(skills, "skill dir", "skill dirs"))
	}
	if n := len(o.opts[optInclude].items); n > 0 {
		add("+" + plural(n, "source", "sources"))
	}

	// The primary is where a run goes unless somebody said otherwise, which is
	// the only case worth a word.
	if server := o.server(); server != nil && server.changed() {
		add("on " + server.selected())
	}

	// Nothing chosen, nothing to say. The marker introduces the answers given,
	// so on its own it introduces nothing and is one more thing on screen that
	// never changes.
	if len(parts) == 0 {
		return ""
	}
	marker, separator := st.chip, st.chip
	if focused && harness.changed() {
		marker = st.chipOn
	}
	return marker.Render("⏵⏵ ") + strings.Join(parts, separator.Render(" · "))
}

// command renders the run invocation the current options describe. It is the
// panel's own documentation: what is on screen is reproducible from a shell,
// and if it is not, the panel is offering something the command cannot.
//
// It spells the run as `discobox run`, the only command that makes one, with
// the prompt as -p: the composer holds one piece of text and sends it as one
// argument, which is exactly what -p is. Trailing words would be inventing a
// tokenization the user did not type.
func (o *optionSet) command(prompt string) string {
	return o.commandFor(o.request(prompt, o.opts[optSource].selected()))
}

// commandFor is that invocation for a request already made: the create card
// previews one whose source is the row under its cursor rather than the one
// the Source row is set to.
func (o *optionSet) commandFor(req RunRequest) string {
	args := []string{"discobox"}
	if req.Server != "" {
		args = append(args, "--server", req.Server)
	}
	// --project names a project on the primary, and a run on another server
	// goes to that server's default (ADR 0116 §4): naming this one would offer
	// a line that fails where it was told to run.
	if p := o.session.Project; p != "" && p != o.session.DefaultProject && req.Server == "" {
		args = append(args, "--project", p)
	}
	// run's own arguments, after the command name; the ones above are global
	// and go in front of it. SOURCE leads, as `run SOURCE` reads, and is "."
	// for the window's own directory. --no-source takes its place.
	run := []string{"run"}
	switch {
	case req.NoSource:
		run = append(run, "--no-source")
	case req.Source != "":
		run = append(run, shellQuote(req.Source))
	default:
		run = append(run, ".")
	}
	for _, word := range req.Prompt {
		run = append(run, "-p", shellQuote(word))
	}
	if req.Harness != "" {
		run = append(run, "--harness", req.Harness)
	}
	// Uncommitted work is carried only when asked for, so only that answer
	// has a flag to write; the window's question has none.
	if req.IncludeDirty == "true" {
		run = append(run, "--include-dirty")
	}
	if req.Detach {
		run = append(run, "-d")
	}
	for _, e := range req.Env {
		run = append(run, "-e", shellQuote(e))
	}
	for _, s := range req.Secret {
		run = append(run, "-s", shellQuote(s))
	}
	for _, i := range req.Include {
		run = append(run, "-i", shellQuote(i))
	}
	if req.NoProjectSkills {
		run = append(run, "--no-project-skills")
	}
	if req.UserSkills != nil {
		if *req.UserSkills {
			run = append(run, "--user-skills")
		} else {
			run = append(run, "--user-skills=false")
		}
	}
	for _, dir := range req.SkillDirs {
		run = append(run, "--skills", shellQuote(dir))
	}
	return strings.Join(append(args, run...), " ")
}

func (o *optionSet) view(st *styles, z *zones, width int, prompt string) string {
	// The same box the dialogs get: this panel stands in place of the window
	// exactly as they do, and two modal surfaces at two sizes read as two
	// different kinds of thing.
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 20)

	var b strings.Builder
	b.WriteString(st.dialogTitle.Render("New Discobox"))
	b.WriteString("\n")
	// The keys, as a key line like every other in the window: the panel is one
	// of the modal surfaces, and a surface where the offers were text and
	// nowhere else they were buttons would be the odd one out.
	z.push(dialogPadLeft, strings.Count(b.String(), "\n")+dialogPadTop)
	b.WriteString(viewHints(st, z, fitHints([]hint{
		says("← → change"),
		pressing("Enter opens the row", "enter"),
		pressing("Esc back to the prompt", "esc"),
	}, hintSep, inner), 0, hintSep))
	z.pop()
	b.WriteString("\n\n")

	// Where the first row lands inside the card: the title, the key line under
	// it, and the blank between them and the rows. Counted off the builder
	// rather than written out, so a line added above the rows moves the marks
	// with it.
	rowsTop := strings.Count(b.String(), "\n") + dialogPadTop

	for i, opt := range o.opts {
		b.WriteString(opt.viewRow(st, z, optionRow{
			value: opt.display(), y: rowsTop + i, inner: inner, cursor: i == o.cursor,
			row:   hit{kind: hitOptionRow, idx: i},
			cycle: hit{kind: hitOptionCycle, idx: i},
		}))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(st.dimText.Render(truncate(o.current().hint, inner)))
	b.WriteString("\n\n")
	b.WriteString(st.dimText.Render("would run"))
	b.WriteString("\n")
	for _, l := range wrap(o.command(prompt), inner) {
		b.WriteString(st.command.Render(l))
		b.WriteString("\n")
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}

// optionLabelW is the label column of an option row, in the panel and on the
// create card both.
const optionLabelW = 21

// optionRow is where one option row is drawn and what a press on it is: the
// panel and the create card draw the same rows, each with marks of its own.
type optionRow struct {
	// value is what the row reads as, which is the option's own display
	// except where the card says the same thing in other words.
	value string
	y     int
	inner int
	// cursor is whether the keyboard is on this row.
	cursor bool
	// row is the press on the row, and cycle the one on its arrows, which is
	// marked once for each direction.
	row, cycle hit
}

// viewRow draws one option row and marks it.
func (o *option) viewRow(st *styles, z *zones, at optionRow) string {
	// The row, and then the arrows over the top of it: they are drawn on
	// the cursor row alone, and each is a press that changes the value the
	// way the arrow key does.
	z.markRow(at.row, at.y, at.inner+2*dialogPadLeft)

	hovered := z.hovering(0, at.y, at.inner, 1)

	bar := " "
	label := st.dimText.Render(pad(o.label, optionLabelW))
	switch {
	case at.cursor:
		bar = st.key.Render("❯")
		label = st.cursorName.Render(pad(o.label, optionLabelW))
	case hovered:
		// Under the pointer: the label says so, and the chevron stays the
		// cursor's, exactly as it does on a menu's rows.
		label = st.hover.Render(pad(o.label, optionLabelW))
	}
	value := at.value
	valueStyle := st.dimText
	switch {
	case o.changed():
		valueStyle = st.chipOn
	case at.cursor:
		valueStyle = st.name
	}
	// A row whose value can be stepped keeps the two columns its arrows
	// need, whether or not they are lit: the arrows are the one affordance
	// the panel depends on, and a value that jumped two cells sideways as
	// the pointer crossed it would be a panel that fidgets. They light on
	// the cursor row, which is what says left and right will work, and on
	// the row under the pointer, which is what says a press will — without
	// that, changing a value with the mouse is click the row, watch
	// nothing happen, then click the arrow that has appeared.
	if o.kind != optChoice && o.kind != optToggle {
		value = valueStyle.Render(truncate(value, max(at.inner-optionLabelW-3, 6)))
		return padANSI(bar+" "+label+" "+value, at.inner)
	}
	left, right := st.dimText.Render("  "), st.dimText.Render("  ")
	if at.cursor || hovered {
		left, right = st.key.Render("‹ "), st.key.Render(" ›")
	}
	value = left + valueStyle.Render(truncate(value, max(at.inner-optionLabelW-7, 6))) + right
	// Both arrow columns are marked on every stepping row, lit or not: the
	// press that lights them is the same press that would use them, and a
	// target that only exists after the pointer has already stopped is one
	// the first click misses.
	valueX := dialogPadLeft + optionLabelW + 3
	back, forward := at.cycle, at.cycle
	back.delta, forward.delta = -1, 1
	z.mark(back, valueX, at.y, 2, 1)
	z.mark(forward, valueX+lipgloss.Width(value)-2, at.y, 2, 1)
	return padANSI(bar+" "+label+" "+value, at.inner)
}

// userSkillsConfigured is the User skills row left to the client's
// configuration, which the request carries as no answer at all.
const userSkillsConfigured = "as configured"

// userSkills is the User skills row as RunRequest.UserSkills: nil while it is
// left to the configuration.
func (o *optionSet) userSkills() *bool {
	switch o.opts[optUserSkills].idx {
	case 1:
		return new(true)
	case 2:
		return new(false)
	}
	return nil
}
