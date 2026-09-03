package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

	// folder is the folder the window is currently working in, chosen in the
	// header. It is where Enter creates from, so switching folders switches
	// what a new sandbox is cut from as well as which ones are listed — the
	// header is one control, not two that happen to look alike. Empty is the
	// session's own directory, which is also what "all folders" falls back to:
	// there is no single folder to create in then.
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
}

// Source is one place the project's discoboxes have been cut from, as the
// Source row offers it.
type Source struct {
	// Value is what `-C` takes: the client directory, or the repository URL.
	Value string
	// Remote marks a repository URL rather than a directory on a client.
	Remote bool
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
const enterSourceChoice = "enter a directory, URL, or DIR@REF…"

// Index into optionSet.opts. The command builder reads by name, so the order
// here is only the order they are shown in.
//
// These are `discobox run`'s flags and nothing else: --harness, --include-dirty,
// --detach, -e and -s, plus -C, the source the sandbox is cut from. A launcher
// that offers options the command does not have is a launcher you cannot
// reproduce from a shell.
//
// The project is not among them. It is set once for the session, the way
// `discobox -p foo` sets it for a shell, so it belongs in the header rather than
// in a panel you would have to open to find out where a sandbox went.
const (
	optHarness = iota
	optDirty
	optDetach
	optEnv
	optSecret
	optSource
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
			hint: "-e · KEY=VALUE, or KEY to pass through from this shell",
		},
		optSecret: {
			label: "Secrets", kind: optMulti,
			hint: "-s · KEY=VALUE, or KEY=<sec_id> to reference an existing secret",
		},
		optSource: {
			label: "Source", kind: optChoice,
			hint: sourceHint,
		},
	}}
	set.rebuildSources()
	return set
}

// sourceHint is what the Source row says: what it sets, and the one thing about
// it that left and right cannot do.
const sourceHint = "-C · where the discobox is cut from · Enter opens the whole list, and a path of your own"

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
// leaving a name the run would be refused for.
func (o *optionSet) setHarnesses(harnesses []Harness) {
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
	if def != "" {
		harness.hint = "--harness · unset is the project default, which is " + def + " · " + HarnessesKeyName + " manages them"
	}
}

// setFolder points the run source at the folder the header has moved to. The
// header is where the folder is chosen, so a source chosen earlier gives way to
// it: leaving a stale override behind would mean the window says one thing and
// creates in another.
func (o *optionSet) setFolder(folder string) {
	o.folder = folder
	o.source = o.sourceDir()
	o.rebuildSources()
}

// setSources takes the sources the project's discoboxes were cut from, off the
// same listing the folder dropdown is built from. What was chosen survives a
// refresh, including a path typed by hand that no discobox has been cut from
// yet.
func (o *optionSet) setSources(known []Source) {
	o.known = known
	o.rebuildSources()
}

// rebuildSources lays the Source row out: the directory this window is running
// in, then every other source the project has been cut from, then "no source".
//
// The order is fixed and does not follow what is chosen. Leading with the
// folder the header is on would reorder the row every time the list followed a
// source to its folder, and left-right would then only ever reach the first two
// entries. Which one counts as unchanged is carried by the row instead, so the
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
		for _, seen := range values {
			if seen == value {
				return
			}
		}
		choices = append(choices, o.sourceChoiceLabel(value))
		values = append(values, value)
	}
	add(o.session.Directory)
	for _, source := range o.known {
		add(source.Value)
	}
	// The folder the header is on, and a source named by hand, are both things
	// the listing cannot be relied on to hold: a folder whose discoboxes were
	// all cut from somewhere else is still a folder you can cut from, and a
	// path typed into the field has to survive the next refresh of the listing.
	add(o.sourceDir())
	add(o.source)
	choices = append(choices, noSourceChoice)
	values = append(values, sourceNone)

	opt.choices, opt.values, opt.unchanged = choices, values, o.sourceDir()
	opt.idx = 0
	for i, value := range values {
		if value == o.source {
			opt.idx = i
			return
		}
	}
}

// sourceChoiceLabel is how one source reads on the row. The directory this
// window is running in wears its branch, the way the header spells it; nothing
// else has a branch that means anything here.
func (o *optionSet) sourceChoiceLabel(value string) string {
	if value == o.session.Directory {
		return o.session.sourceLabel()
	}
	return value
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

// sourceFolder is where a discobox cut from the chosen source is filed, and so
// which folder the list follows the source to. A local directory is its own
// folder. A remote URL and "no source" have none, so a discobox from either is
// filed under the directory this window is running in.
func (o *optionSet) sourceFolder() string {
	switch value := o.opts[optSource].selected(); {
	case value == sourceNone, o.remoteSource(value):
		return o.session.Directory
	default:
		return sourceDirectory(value)
	}
}

// remoteSource reports whether a value is a repository URL rather than a
// directory. The listing says so per source rather than this package parsing
// the string, because what counts as a remote is the creation path's to decide
// and not the window's.
func (o *optionSet) remoteSource(value string) bool {
	for _, source := range o.known {
		if source.Value == value {
			return source.Remote
		}
	}
	return false
}

// followSource moves the panel's folder to where the chosen source files its
// discoboxes, and returns it for the list and the header to follow. This is the
// source moving the header rather than the header moving the source, so the
// source itself is left exactly as it was chosen.
func (o *optionSet) followSource() string {
	folder := o.sourceFolder()
	o.folder = folder
	o.rebuildSources()
	return folder
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
		if source.Value == o.source {
			return ""
		}
	}
	return o.source
}

// sourceChosenMsg carries the dropdown's answer back to the live model, for the
// same reason folderChosenMsg does: the dialog closed over the model by value.
// enter is the row that is not an answer but a request for the input field.
type sourceChosenMsg struct {
	source string
	enter  bool
}

// sourceDialog is the Source row opened out: every source the project has been
// cut from, "no source", and the way to name one the listing has never seen.
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
		return "the folder this window is showing"
	case o.remoteSource(value):
		return "cloned by the discobox itself"
	default:
		return ""
	}
}

// sourceDirectory is the directory half of a `DIR@REF` value: the ref says
// which commit to cut from, and the folder a discobox is filed under is the
// directory either way.
func sourceDirectory(value string) string {
	if i := strings.LastIndex(value, "@"); i > 0 {
		return value[:i]
	}
	return value
}

// sourceDir is the directory Enter creates in: the folder the header is on, or
// the session's own when the header is on every folder at once.
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
	if o.sourceDir() == o.session.Directory {
		return o.session.sourceLabel()
	}
	return o.sourceDir()
}

// sourceLabel is the directory and ref a sandbox is cut from by default, in the
// spelling the header uses.
func (s Session) sourceLabel() string {
	if s.Branch == "" {
		return s.Directory
	}
	return s.Directory + " @ " + s.Branch
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
// arguments, with the prompt from the composer.
func (o *optionSet) request(prompt string) RunRequest {
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
	// The folder the header is on is what the source row leads with, and naming
	// the session's own directory would only repeat the CLI's default — so that
	// one case emits no -C and `discobox run` resolves it the way it always does.
	switch source := o.opts[optSource].selected(); {
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

// command renders the `discobox run` invocation the current options describe. It
// is the panel's own documentation: what is on screen is reproducible from a
// shell, and if it is not, the panel is offering something the command cannot.
func (o *optionSet) command(prompt string) string {
	req := o.request(prompt)
	args := []string{"discobox"}
	if p := o.session.Project; p != "" && p != o.session.DefaultProject {
		args = append(args, "--project", p)
	}
	if req.Source != "" {
		args = append(args, "-C", shellQuote(req.Source))
	}
	args = append(args, "run")
	if req.NoSource {
		args = append(args, "--no-source")
	}
	if req.Harness != "" {
		args = append(args, "--harness", req.Harness)
	}
	if req.IncludeDirty != "" {
		args = append(args, "--include-dirty="+req.IncludeDirty)
	}
	if req.Detach {
		args = append(args, "-d")
	}
	for _, e := range req.Env {
		args = append(args, "-e", shellQuote(e))
	}
	for _, s := range req.Secret {
		args = append(args, "-s", shellQuote(s))
	}
	if len(req.Prompt) > 0 {
		args = append(args, "--")
		for _, word := range req.Prompt {
			args = append(args, shellQuote(word))
		}
	}
	return strings.Join(args, " ")
}

func (o *optionSet) view(st *styles, z *zones, width int, prompt string) string {
	// The same box the dialogs get: this panel stands in place of the window
	// exactly as they do, and two modal surfaces at two sizes read as two
	// different kinds of thing.
	boxWidth := dialogWidth(width)
	inner := max(boxWidth-dialogChromeWidth, 20)

	var b strings.Builder
	b.WriteString(st.dialogTitle.Render("Run Options"))
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

	labelW := 21
	for i, opt := range o.opts {
		// The row, and then the arrows over the top of it: they are drawn on
		// the cursor row alone, and each is a press that changes the value the
		// way the arrow key does.
		z.markRow(hit{kind: hitOptionRow, idx: i}, rowsTop+i, inner+2*dialogPadLeft)

		hovered := z.hovering(0, rowsTop+i, inner, 1)

		bar := " "
		label := st.dimText.Render(pad(opt.label, labelW))
		switch {
		case i == o.cursor:
			bar = st.key.Render("❯")
			label = st.cursorName.Render(pad(opt.label, labelW))
		case hovered:
			// Under the pointer: the label says so, and the chevron stays the
			// cursor's, exactly as it does on a menu's rows.
			label = st.hover.Render(pad(opt.label, labelW))
		}
		value := opt.display()
		valueStyle := st.dimText
		switch {
		case opt.changed():
			valueStyle = st.chipOn
		case i == o.cursor:
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
		steps := opt.kind == optChoice || opt.kind == optToggle
		if steps {
			left, right := st.dimText.Render("  "), st.dimText.Render("  ")
			if i == o.cursor || hovered {
				left, right = st.key.Render("‹ "), st.key.Render(" ›")
			}
			value = left + valueStyle.Render(truncate(value, max(inner-labelW-7, 6))) + right
		} else {
			value = valueStyle.Render(truncate(value, max(inner-labelW-3, 6)))
		}
		row := bar + " " + label + " " + value
		if steps {
			// Both arrow columns are marked on every stepping row, lit or not:
			// the press that lights them is the same press that would use
			// them, and a target that only exists after the pointer has
			// already stopped is one the first click misses.
			valueX := dialogPadLeft + labelW + 3
			z.mark(hit{kind: hitOptionCycle, idx: i, delta: -1}, valueX, rowsTop+i, 2, 1)
			z.mark(hit{kind: hitOptionCycle, idx: i, delta: 1}, valueX+lipgloss.Width(value)-2, rowsTop+i, 2, 1)
		}
		b.WriteString(padANSI(row, inner))
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
