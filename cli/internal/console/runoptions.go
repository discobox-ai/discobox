package console

import (
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sourceref"
)

// The run options: every flag `discobox run` takes, as the rows a create is
// made from (ADR 26-11-07-876 §1). What each row offers, what it leads with,
// what counts as unchanged, the sources a create can be cut from, and the
// request and command they add up to are the core's; the cursor over them and
// how they are drawn are each renderer's — the TUI's panel and create card,
// the desktop's sheets (CreateForm, OptionsForm).

// OptionKind is how a row takes its value.
type OptionKind int

const (
	// OptionChoice is one of a list, stepped through.
	OptionChoice OptionKind = iota
	// OptionToggle is on or off.
	OptionToggle
	// OptionList is a repeated flag (-e, -s, --skills, -i): values added one
	// at a time.
	OptionList
)

// RunOption is one row.
type RunOption struct {
	Label   string
	Kind    OptionKind
	Choices []string
	// Values are what the choices stand for, when what a choice is called is
	// not what the command takes. Nil leaves every choice its own value, which
	// is every row but the source, the pool and the server.
	Values []string
	// Unchanged is the value this row counts as the CLI's own default, for a
	// row whose choices are not in a fixed order with the default leading.
	// Empty leaves the default at index zero, which is every row but the
	// source — whose default is wherever the header has moved to.
	Unchanged string
	// Index is the chosen choice.
	Index int
	// Value is a toggle's: "on" or "off".
	Value string
	// Items are a repeated flag's values, in order.
	Items []string
	Hint  string
	// Example is what the field a repeated flag's value is typed into shows
	// while it is empty.
	Example string
	// Checked marks a repeated flag whose values name sources, which are
	// checked (DataSource.ResolveSource) the way a source typed by hand is
	// before the row takes them.
	Checked bool
}

// Changed reports whether the option differs from the CLI's own default, which
// is what decides if it shows up in the summary and in the command preview.
func (o *RunOption) Changed() bool {
	switch o.Kind {
	case OptionChoice:
		if o.Unchanged != "" {
			return o.Selected() != o.Unchanged
		}
		return o.Index != 0
	case OptionToggle:
		return o.Value == "on"
	case OptionList:
		return len(o.Items) > 0
	}
	return false
}

// Selected is the value the current choice stands for: the choice itself
// unless the row carries values of its own.
func (o *RunOption) Selected() string {
	if o.Index < 0 || o.Index >= len(o.Choices) {
		return ""
	}
	if o.Values != nil {
		return o.Values[o.Index]
	}
	return o.Choices[o.Index]
}

// Display is how the row's value reads.
func (o *RunOption) Display() string {
	switch o.Kind {
	case OptionChoice:
		if len(o.Choices) == 0 {
			return ""
		}
		return o.Choices[o.Index]
	case OptionToggle:
		if o.Value == "on" {
			return "yes"
		}
		return "no"
	case OptionList:
		if len(o.Items) == 0 {
			return "(none)"
		}
		return strings.Join(o.Items, "  ")
	}
	return ""
}

// Cycle steps a choice or flips a toggle; a list has nothing to step.
func (o *RunOption) Cycle(delta int) {
	switch o.Kind {
	case OptionChoice:
		if len(o.Choices) == 0 {
			return
		}
		o.Index = (o.Index + delta + len(o.Choices)) % len(o.Choices)
	case OptionToggle:
		if o.Value == "on" {
			o.Value = "off"
		} else {
			o.Value = "on"
		}
	}
}

// RunOptions is the rows, and what they are read against: the session, the
// folder the window is working in and the sources its project has been cut
// from. The project and the directory come from the session rather than the
// rows, and are carried only so the command preview can show the flags it
// would need.
type RunOptions struct {
	// GitPolicyLoading and GitPolicyError are where the client's Git
	// credential preference stands, which the command preview names.
	GitPolicyLoading bool
	GitPolicyError   bool
	// Opts are the rows, by OptHarness and the rest.
	Opts    []*RunOption
	Session Session

	// Folder is the source of the folder the window is currently working in,
	// chosen in the header: the directory or URL a create from it cuts from. It
	// is where Enter creates from, so switching folders switches what a new
	// sandbox is cut from as well as which ones are listed — the header is one
	// control, not two that happen to look alike. Empty is the session's own
	// directory, which is also what "all folders" falls back to: there is no
	// single folder to create in then.
	Folder string

	// known are the sources the project's discoboxes were cut from, newest
	// first, as the listing reports them. They are what the Source row offers
	// besides the folder itself: the places this project is worked in are
	// exactly the ones already holding something.
	known []Source
	// source is what the Source row is set to: a directory, a repository URL,
	// SourceNone, or empty for the folder the header is on. It is kept here
	// rather than read back off the row's choice, because the row is rebuilt
	// whenever the folder or the listing moves under it.
	source string
	// Named is the last source typed by hand, as the check of it reported it.
	// The listing does not hold it, so where a discobox cut from it is filed
	// is kept here for the list to follow it there.
	Named Source
	// pools are the server's pools as last listed, which the Pool row is
	// built from; see SetPools.
	pools []Pool
	// lastCut is the source the last create card was answered with, in this
	// window or — through Session.LastSource — the last one on this machine.
	// It leads the Source row, and answering a card sets the row to it, so
	// creating again from the same place is one Enter. Empty until a card has
	// ever been answered.
	lastCut string
}

// SourceNone is what the Source row carries for a discobox with nothing
// checked out in it. It is a sentinel rather than the empty string because
// empty already means "the folder the header is on", and the two are different
// answers; the NUL keeps it from ever colliding with a path or a URL.
const SourceNone = "\x00no-source"

// EnterSourceChoice is the source list's last row: the one entry that is not a
// source but a way to name one the listing has never seen. It is only in the
// list, never in the left-right cycle, because cycling onto it would mean
// stopping on a value that is not one.
const EnterSourceChoice = "enter a directory, URL, or owner/repo, with @REF and //PATH…"

// Index into RunOptions.Opts. The command builder reads by name, so the order
// here is only the order they are shown in.
//
// These are `discobox run`'s flags and nothing else: --harness, --pool,
// --include-dirty, --detach, -e and -s, SOURCE, the source the sandbox is cut
// from, and what is brought in beside it — --user-skills, --skills and -i. A
// launcher that offers options the command does not have is a launcher you
// cannot reproduce from a shell.
//
// The project is not among them. It is set once for the session, the way
// DISCOBOX_PROJECT sets it for a shell, so it belongs in the header rather than
// in a panel you would have to open to find out where a sandbox went.
const (
	OptHarness = iota
	OptDirty
	OptDetach
	OptEnv
	OptSecret
	OptSource
	OptProjectSkills
	OptUserSkills
	OptSkills
	OptInclude
	// OptPool is where the discobox is created, beside the server it is on.
	OptPool
	// OptServer is last because it is the one row that is not always in the
	// list: it exists only when there is a server to choose (ADR 0116 §5), and
	// the rows before it keep their places either way. See RunOptions.Server.
	// The Pool row is always in the list and drawn only when there is a pool
	// to choose (Shown).
	OptServer
)

// UnsetHarness is what the harness row says when nothing has been chosen on
// it. It is spelled out rather than left blank, because a list with an empty
// row in it reads as a bug.
//
// There is no "none" among the choices. Running without a coding harness is the
// `shell` harness (ADR 0043), which is one of the project's like any other, so
// an entry meaning the same thing would be a second way to say it.
const UnsetHarness = "(default)"

// harnessHint is what the harness row says before the harnesses are listed.
// What manages them is a renderer's to add, with its own way there.
const harnessHint = "--harness"

// NewRunOptions is the rows as a window opens: every default, the session's
// servers, and its directory to cut from.
func NewRunOptions(session Session) *RunOptions {
	// The choices are the project's harnesses, which are loaded on their own
	// and arrive here through SetHarnesses. Until they do the row is empty
	// rather than offering "none (shell)" as if it were the default: a panel
	// that names the wrong harness for a moment is worse than one that names
	// none.
	set := &RunOptions{Session: session, Opts: []*RunOption{
		OptHarness: {
			Label: "Harness", Kind: OptionChoice,
			Hint: harnessHint,
		},
		OptDirty: {
			Label: "Uncommitted changes", Kind: OptionChoice,
			Choices: []string{"auto", "include", "exclude"},
			Hint:    "--include-dirty · auto asks when the working tree is dirty",
		},
		OptDetach: {
			Label: "Detach", Kind: OptionToggle, Value: "off",
			Hint: "-d · create the discobox and print it, without attaching",
		},
		OptEnv: {
			Label: "Env", Kind: OptionList,
			Hint:    "-e · KEY=VALUE, or KEY to pass through from this shell",
			Example: "KEY=VALUE",
		},
		OptSecret: {
			Label: "Secrets", Kind: OptionList,
			Hint:    "-s · KEY from this shell, KEY=@FILE, or KEY=VALUE; @HOST sends it to HOST alone",
			Example: "KEY",
		},
		OptSource: {
			Label: "Source", Kind: OptionChoice,
			Hint: SourceHint,
		},
		OptProjectSkills: {
			// On is what every create does unless told otherwise: the
			// project's skills repositories, copied in ahead of any --skills
			// (ADR 26-10-14-894 §2).
			Label: "Project skills", Kind: OptionChoice,
			Choices: []string{"on", "off"},
			Hint:    "--no-project-skills · the project's skills repositories, added ahead of any --skills",
		},
		OptUserSkills: {
			// Left alone it is client.yaml's run.userSkills, which is what
			// `discobox run` gives a create that names neither way.
			Label: "User skills", Kind: OptionChoice,
			Choices: []string{UserSkillsConfigured, "on", "off"},
			Hint:    "--user-skills · install your own ~/.claude/skills and ~/.agents/skills in the discobox",
		},
		OptSkills: {
			Label: "Skills", Kind: OptionList,
			Hint:    "--skills · a directory of skills, each a subdirectory holding a SKILL.md, or a Git URL[@REF][//PATH]",
			Example: "./team-skills",
		},
		OptInclude: {
			Label: "Additional sources", Kind: OptionList,
			Hint:    "-i · another directory, URL, or owner/repo, with @REF, brought into the same discobox",
			Example: "../other-repo", Checked: true,
		},
		OptPool: {
			// Like the harness row, the choices are the server's and arrive
			// through SetPools; until they do, the row is the default alone.
			Label: "Pool", Kind: OptionChoice,
			Choices: []string{UnsetPool}, Values: []string{""},
			Hint: poolHint,
		},
	}}
	if len(session.Servers) > 1 {
		// The primary leads, so the common case is index zero and emits no
		// flag, and it says it is the primary: the name alone does not say
		// which of them a run goes to when nothing is chosen.
		choices := append([]string{session.Servers[0] + " (primary)"}, session.Servers[1:]...)
		set.Opts = append(set.Opts, &RunOption{
			Label: "Server", Kind: OptionChoice,
			Choices: choices,
			Values:  append([]string(nil), session.Servers...),
			Hint:    serverHint,
		})
	}
	set.rebuildSources()
	return set
}

// Clone is a copy whose rows can be changed without touching these.
func (o *RunOptions) Clone() *RunOptions {
	c := *o
	c.Opts = make([]*RunOption, len(o.Opts))
	for i, opt := range o.Opts {
		row := *opt
		row.Choices = slices.Clone(opt.Choices)
		row.Values = slices.Clone(opt.Values)
		row.Items = slices.Clone(opt.Items)
		c.Opts[i] = &row
	}
	c.known = slices.Clone(o.known)
	c.pools = slices.Clone(o.pools)
	return &c
}

// serverHint is what the Server row says.
const serverHint = "--server · the server the discobox is created on"

// SourceHint is what the Source row says: what it sets. How a renderer opens
// the whole list, and takes a path of its own, is that renderer's to add.
const SourceHint = "SOURCE · where the discobox is cut from"

// Server is the Server row, or nil when there is only the primary to create
// on.
func (o *RunOptions) Server() *RunOption {
	if len(o.Opts) > OptServer {
		return o.Opts[OptServer]
	}
	return nil
}

// SetServer points a create at the server the header has moved to, for the
// same reason SetFolder points the source at its folder: the header is where
// the window says which server it is working on, and a run option left behind
// it would create somewhere the window does not say.
//
// A name the row does not carry — and the empty name the header's "all
// servers" holds — leaves it on the primary, which is where a create with no
// --server at all goes (ADR 0116 §5): every server at once is not an answer to
// which one a new discobox belongs on.
func (o *RunOptions) SetServer(name string) {
	row := o.Server()
	if row == nil {
		return
	}
	row.Index = 0
	for i, value := range row.Values {
		if value == name {
			row.Index = i
			return
		}
	}
}

// Prefill sets the rows to what a request already says, for the card
// `discobox run` asks its source on: a flag given on the command line is an
// answer, and the card opens holding it rather than asking it again. A harness
// the listing does not offer is added to the row rather than dropped, so the
// create meets the server's answer about it rather than a different harness.
func (o *RunOptions) Prefill(req RunRequest) {
	if req.Harness != "" {
		harness := o.Opts[OptHarness]
		i := slices.Index(harness.Choices, req.Harness)
		if i < 0 {
			harness.Choices = append(harness.Choices, req.Harness)
			i = len(harness.Choices) - 1
		}
		harness.Index = i
	}
	if req.Pool != "" {
		o.ChoosePool(req.Pool)
	}
	switch req.IncludeDirty {
	case "true":
		o.Opts[OptDirty].Index = 1
	case "false":
		o.Opts[OptDirty].Index = 2
	}
	if req.NoProjectSkills {
		o.Opts[OptProjectSkills].Index = 1
	}
	if req.UserSkills != nil {
		o.Opts[OptUserSkills].Index = 2
		if *req.UserSkills {
			o.Opts[OptUserSkills].Index = 1
		}
	}
	o.Opts[OptEnv].Items = append([]string(nil), req.Env...)
	o.Opts[OptSecret].Items = append([]string(nil), req.Secret...)
	o.Opts[OptSkills].Items = append([]string(nil), req.SkillDirs...)
	o.Opts[OptInclude].Items = append([]string(nil), req.Include...)
}

// SetHarnesses rebuilds the harness choices from the harnesses the window
// knows about. The project default leads, so the common case is index zero
// and emits no flag at all — the same shape every other option here has.
//
// The listing is the one source of what harnesses there are: enabling one on
// the harnesses screen puts it here without the window being reopened, and
// disabling the one that was chosen falls back to the default rather than
// leaving a name the run would be refused for. Every harness runs on every
// pool: a harness names no platform and no image kind, which are its pool's
// (ADR 26-11-01-122).
func (o *RunOptions) SetHarnesses(harnesses []Harness) {
	harness := o.Opts[OptHarness]
	chosen := ""
	if harness.Index > 0 && harness.Index < len(harness.Choices) {
		chosen = harness.Choices[harness.Index]
	}

	var def string
	for _, harness := range harnesses {
		if harness.Default {
			def = harness.FlagName()
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
		choices = append(choices, UnsetHarness)
	}
	for _, harness := range harnesses {
		if name := harness.FlagName(); name != "" && name != def {
			choices = append(choices, name)
		}
	}

	harness.Choices = choices
	harness.Index = 0
	for i, choice := range choices {
		if choice == chosen {
			harness.Index = i
		}
	}
	harness.Hint = "--harness · no project default, so pick one"
	if def != "" {
		harness.Hint = "--harness · unset is the project default, which is " + def
	}
}

// HarnessesListed reports whether the harness row has been built from a
// listing yet.
func (o *RunOptions) HarnessesListed() bool { return len(o.Opts[OptHarness].Choices) > 0 }

// UnsetPool is what the Pool row leads with when the project names no default
// pool: a create with no --pool goes wherever the server places it.
const UnsetPool = "(default)"

// poolHint is what the Pool row says before the listing has said which pool
// the default is.
const poolHint = "--pool · the pool the discobox is created on, which decides the platform it runs on"

// SetPools rebuilds the Pool row from the server's pools. The project default
// leads, so the common case is index zero and emits no flag, as on the harness
// row; the rest follow in the order the listing gives them.
//
// The row is drawn only when there is a pool to choose (Shown): a project with
// one pool, which is most of them, never hears the word, as the machine line
// does not use it (Resources). The create card hides it the same way.
//
// What was chosen survives the rebuild, matched by ID — through the pool the
// last listing said it was, so a rename does not lose it — or by name: a
// refresh is not a reason to move a create elsewhere. A pool the listing does
// not hold — one `discobox run --pool` named — stays on the row rather than
// being dropped, so the create meets the server's answer about it rather than
// another pool.
func (o *RunOptions) SetPools(pools []Pool) {
	row := o.Opts[OptPool]
	chosen := row.Selected()
	for _, pool := range o.pools {
		if chosen != "" && chosen == PoolSelector(pool, o.pools) {
			chosen = pool.ID
		}
	}
	o.pools = pools

	choices, values := []string{UnsetPool}, []string{""}
	hint := poolHint
	for _, pool := range pools {
		if pool.Default {
			choices[0] = PoolLabel(pool)
			hint = "--pool · unset is the project default, which is " + PoolLabel(pool) + platformNote(pool)
		}
	}
	for _, pool := range pools {
		if !pool.Default {
			choices = append(choices, PoolLabel(pool))
			values = append(values, PoolSelector(pool, pools))
		}
	}

	row.Index = 0
	if chosen != "" {
		i := -1
		for _, pool := range pools {
			if chosen == pool.ID || (i < 0 && chosen == PoolSelector(pool, pools)) {
				// The default named outright is the default.
				i = 0
				if !pool.Default {
					i = slices.Index(values, PoolSelector(pool, pools))
				}
			}
		}
		if i < 0 {
			choices, values = append(choices, chosen), append(values, chosen)
			i = len(values) - 1
		}
		row.Index = i
	}
	row.Choices, row.Values, row.Hint = choices, values, hint
}

// ChoosePool sets the Pool row to a pool named by ID or name, before or after
// the listing has arrived: matched against the listing when there is one, and
// kept as named when there is none yet, for SetPools to match when it lands.
func (o *RunOptions) ChoosePool(value string) {
	row := o.Opts[OptPool]
	row.Choices, row.Values = append(row.Choices, value), append(row.Values, value)
	row.Index = len(row.Values) - 1
	if o.pools != nil {
		o.SetPools(o.pools)
	}
}

// ClearPools puts the Pool row back on the default with nothing listed, for a
// change of server: one server's pool is not another's.
func (o *RunOptions) ClearPools() {
	row := o.Opts[OptPool]
	row.Index = 0
	o.SetPools(nil)
}

// PoolLabel is how a pool reads on the row: its name, or its ID when it has
// none.
func PoolLabel(pool Pool) string {
	if pool.Name != "" {
		return pool.Name
	}
	return pool.ID
}

// platformNote is the platform a pool hosts, as the end of a hint.
func platformNote(pool Pool) string {
	if pool.Platform == "" {
		return ""
	}
	return " (" + pool.Platform + ")"
}

// PoolSelector is what --pool takes for a pool: its name, which is what the
// command preview is read for, unless another pool shares it, where only the
// ID says which.
func PoolSelector(pool Pool, pools []Pool) string {
	if pool.Name == "" {
		return pool.ID
	}
	for _, other := range pools {
		if other.ID != pool.ID && other.Name == pool.Name {
			return pool.ID
		}
	}
	return pool.Name
}

// SetFolder points the run source at the folder the header has moved to. The
// header is where the folder is chosen, so a source chosen earlier gives way
// to it: leaving a stale override behind would mean the window says one thing
// and creates in another.
//
// "All folders" names no place to cut from, so there the row goes to the last
// create's source when there is one, rather than falling back silently to the
// directory the window happens to be running in.
func (o *RunOptions) SetFolder(folder string) {
	o.Folder = folder
	o.source = o.SourceDir()
	if folder == "" && o.lastCut != "" {
		o.source = o.lastCut
	}
	o.rebuildSources()
}

// SetSources takes the sources the project's discoboxes were cut from, off the
// same listing the header filter's folders are built from. What was chosen
// survives a refresh, including a path typed by hand that no discobox has been
// cut from yet.
func (o *RunOptions) SetSources(known []Source) {
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
// then only ever reach the first two entries. Which one counts as unchanged is
// carried by the row instead, so the panel can say "this is not where the
// header is" without the order saying it.
func (o *RunOptions) rebuildSources() {
	opt := o.Opts[OptSource]
	var choices, values []string
	add := func(value string) {
		// "No source" is appended once at the end, where it belongs; it is
		// also what o.source holds when it is chosen, so it has to be kept
		// out of the sources themselves.
		if strings.TrimSpace(value) == "" || value == SourceNone {
			return
		}
		// One source is one choice, however each discobox spelled it.
		for _, seen := range values {
			if SameSource(seen, value) {
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
	add(o.Session.Directory)
	for _, source := range o.known {
		add(source.Value)
	}
	// The folder the header is on and a source named by hand are things the
	// listing cannot be relied on to hold either: a folder whose discoboxes
	// were all cut from somewhere else is still a folder you can cut from, and
	// a path typed into the field has to survive the next refresh of the
	// listing.
	add(o.SourceDir())
	add(o.source)
	choices = append(choices, NoSourceChoice)
	values = append(values, SourceNone)

	opt.Choices, opt.Values, opt.Unchanged = choices, values, o.SourceDir()
	opt.Index = 0
	for i, value := range values {
		if SameSource(value, o.source) {
			opt.Index = i
			return
		}
	}
}

// sourceChoiceLabel is how one source reads on the row. The directory this
// window is running in wears its branch, the way the header spells it; nothing
// else has a branch that means anything here.
func (o *RunOptions) sourceChoiceLabel(value string) string {
	if SameSource(value, o.Session.Directory) {
		return o.Session.SourceLabel()
	}
	return sourceref.Display(value, o.Session.Home)
}

// CutFrom records the source a create was cut from and sets the row to it, so
// the next create opens there whatever the header is on. The header is not
// moved: the answer is where a discobox comes from, not a filter.
func (o *RunOptions) CutFrom(source string) {
	o.lastCut = source
	o.ChooseSource(source)
}

// OpenOnLastCut sets the row to where the last window's last create was cut
// from (Session.LastSource).
func (o *RunOptions) OpenOnLastCut(source string) {
	if source == "" {
		o.lastCut = source
		o.rebuildSources()
		return
	}
	o.CutFrom(source)
}

// CycleSource moves the Source row and records what it landed on, since the
// row is rebuilt from that rather than from where its choice happens to sit.
func (o *RunOptions) CycleSource(delta int) {
	opt := o.Opts[OptSource]
	opt.Cycle(delta)
	o.ChooseSource(opt.Selected())
}

// ChooseSource sets the row to a source, from the cycle, from the list, or
// from the input field. Nothing entered is the folder the header is on, which
// is what the field was offering as its placeholder.
func (o *RunOptions) ChooseSource(value string) {
	if strings.TrimSpace(value) == "" {
		value = o.SourceDir()
	}
	o.source = value
	o.rebuildSources()
}

// SourceFolder is where a discobox cut from the chosen source is filed (ADR
// 0111), and so which folder the list follows the source to. The window's own
// source files under the window's own folder, and so does "no source", since
// this machine's discoboxes with no source are in every one of its folders. A
// directory or a URL files under its own, as the listing — or the check of a
// source typed by hand — reported it, and is in its GitHub repository's folder
// when it resolved to one. A source nothing has reported on is not followed
// anywhere.
func (o *RunOptions) SourceFolder() (Folder, bool) {
	value := o.Opts[OptSource].Selected()
	if value == SourceNone || SameSource(value, o.Session.Directory) {
		return o.Session.Folder(), true
	}
	of := func(source Source) (Folder, bool) {
		if !SameSource(source.Value, value) || source.OriginKey == "" {
			return Folder{}, false
		}
		// A directory's folder is the directory, whatever ref was named with
		// it: the ref is the source row's to say, not the folder's.
		dir := value
		if !source.Remote && source.Directory != "" {
			dir = source.Directory
		}
		if source.RepositoryKey != "" {
			return Folder{Key: source.RepositoryKey, Label: RepositoryLabel(source.RepositoryKey), Source: dir, Local: true}, true
		}
		return Folder{Key: source.OriginKey, Label: sourceref.Display(dir, o.Session.Home), Source: dir, Local: true}, true
	}
	if f, ok := of(o.Named); ok {
		return f, true
	}
	for _, source := range o.known {
		if f, ok := of(source); ok {
			return f, true
		}
	}
	return Folder{}, false
}

// NameSource sets the row to a source typed by hand and checked, keeping what
// the check said about where a discobox cut from it is filed.
func (o *RunOptions) NameSource(source Source) {
	o.Named = source
	o.ChooseSource(source.Value)
}

// remoteSource reports whether a value is a repository URL rather than a
// directory. The listing — or the check of a source typed by hand — says so
// per source rather than this package resolving the string, because whether
// a shorthand is a directory here is the creation path's to decide and not
// the window's.
func (o *RunOptions) remoteSource(value string) bool {
	if SameSource(o.Named.Value, value) {
		return o.Named.Remote
	}
	for _, source := range o.known {
		if SameSource(source.Value, value) {
			return source.Remote
		}
	}
	return false
}

// FollowSource moves the folder to where the chosen source files its
// discoboxes, and returns it for the list and the header to follow — or
// reports that nothing says where that is, and the header stays where it is.
// This is the source moving the header rather than the header moving the
// source, so the source itself is left exactly as it was chosen.
func (o *RunOptions) FollowSource() (Folder, bool) {
	f, ok := o.SourceFolder()
	if !ok {
		return Folder{}, false
	}
	o.Folder = f.Source
	o.rebuildSources()
	return f, true
}

// TypedSource is what the input field opens holding: a source named by hand
// rather than one the listing offered, so re-opening the field to fix a typo
// does not start from nothing. A choice made off the list leaves it empty and
// the field shows the folder as its placeholder.
func (o *RunOptions) TypedSource() string {
	if o.source == SourceNone || o.source == o.SourceDir() {
		return ""
	}
	for _, source := range o.known {
		if SameSource(source.Value, o.source) {
			return ""
		}
	}
	return o.source
}

// SourceDetail is what each source choice is worth knowing beyond its own
// name.
func (o *RunOptions) SourceDetail(value string) string {
	switch {
	case value == SourceNone:
		return "nothing checked out — an empty discobox"
	case value == o.SourceDir():
		if o.Folder == "" {
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
// (SourceKey). The Source row's own no-source answer is its own key.
func sourceKey(value string) string {
	if value == SourceNone {
		return value
	}
	return SourceKey(value)
}

// SameSource reports whether two values name one source.
func SameSource(a, b string) bool {
	return a == b || sourceKey(a) == sourceKey(b)
}

// SourceDir is what Enter creates from: the folder the header is on, or the
// window's own source when the header is on every folder at once.
func (o *RunOptions) SourceDir() string {
	if o.Folder == "" {
		return o.Session.Directory
	}
	return o.Folder
}

// SourceLabel is that directory as the header spells it, with the branch on it
// only where the branch means something — the directory this window is
// actually running in.
func (o *RunOptions) SourceLabel() string {
	if SameSource(o.SourceDir(), o.Session.Directory) {
		return o.Session.SourceLabel()
	}
	return sourceref.Display(o.SourceDir(), o.Session.Home)
}

// Shown reports whether a row is offered. Every row is but the pool's, which
// is there only when there is a pool to choose (see SetPools).
func (o *RunOptions) Shown(i int) bool {
	return i != OptPool || len(o.Opts[OptPool].Choices) > 1
}

// Request is what a create asks for: the options as `discobox run`'s
// arguments, with the prompt from the composer. The source is passed rather
// than read off the row, because the answer to a create's own question is cut
// from without moving the row (the create card); everything else passes what
// the row has selected.
func (o *RunOptions) Request(prompt, source string) RunRequest {
	req := RunRequest{
		Detach: o.Opts[OptDetach].Changed(),
		Env:    append([]string(nil), o.Opts[OptEnv].Items...),
		Secret: append([]string(nil), o.Opts[OptSecret].Items...),
	}
	// One argument, because the composer holds one piece of text and splitting
	// it would be inventing tokens the user did not type.
	if text := strings.TrimSpace(prompt); text != "" {
		req.Prompt = []string{text}
	}
	// The window's own directory is left empty: the create cuts from where the
	// window runs, and the preview writes it as ".".
	switch {
	case source == SourceNone:
		req.NoSource = true
	case source != o.Session.Directory:
		req.Source = source
	}
	if h := o.Opts[OptHarness]; h.Changed() {
		req.Harness = h.Choices[h.Index]
	}
	if pool := o.Opts[OptPool]; pool.Changed() {
		req.Pool = pool.Selected()
	}
	switch o.Opts[OptDirty].Index {
	case 1:
		req.IncludeDirty = "true"
	case 2:
		req.IncludeDirty = "false"
	}
	req.NoProjectSkills = o.Opts[OptProjectSkills].Changed()
	req.UserSkills = o.UserSkills()
	req.SkillDirs = append([]string(nil), o.Opts[OptSkills].Items...)
	req.Include = append([]string(nil), o.Opts[OptInclude].Items...)
	if server := o.Server(); server != nil && server.Changed() {
		req.Server = server.Selected()
	}
	return req
}

// Chip is one part of the summary of what a create will do: its words, and
// whether it is an answer somebody gave rather than a default.
type Chip struct {
	Text   string
	Chosen bool
}

// Summary is the line under the composer — Claude Code's "⏵⏵ bypass
// permissions on" slot: the harness always, since it is the most
// consequential part of what Enter will run, and every other row only where
// it was answered. Auto is what an option already does, and a line that names
// every default is a line you stop reading for the one thing on it that was
// chosen. Env and secrets are counts. Nothing chosen and no harness listed is
// nothing to say.
func (o *RunOptions) Summary() []Chip {
	var chips []Chip
	add := func(text string) { chips = append(chips, Chip{Text: text, Chosen: true}) }

	if harness := o.Opts[OptHarness]; harness.Display() != "" {
		chips = append(chips, Chip{Text: harness.Display(), Chosen: harness.Changed()})
	}
	if pool := o.Opts[OptPool]; pool.Changed() {
		add("pool " + pool.Display())
	}
	switch o.Opts[OptDirty].Index {
	case 1:
		add("+dirty")
	case 2:
		add("clean")
	}
	// Attaching is what happens unless you say otherwise, and a line that
	// says what was always going to happen is a line you stop reading.
	if o.Opts[OptDetach].Changed() {
		add("detached")
	}
	if n := len(o.Opts[OptEnv].Items); n > 0 {
		add(count(n, "env", "env"))
	}
	if n := len(o.Opts[OptSecret].Items); n > 0 {
		add(count(n, "secret", "secrets"))
	}
	// The source is only worth a chip when it is not where the window says it
	// is. The header already names the folder the window is working in, and a
	// strip that repeats it teaches you to stop reading the strip — which is
	// exactly what the row's leading choice is.
	if source := o.Opts[OptSource]; source.Changed() {
		add(source.Display())
	}
	// What is brought in beside the source, as counts for the same reason env
	// and secrets are: the summary says there is something, the rows say what.
	if o.Opts[OptProjectSkills].Changed() {
		add("no project skills")
	}
	skills := len(o.Opts[OptSkills].Items)
	user := o.UserSkills()
	switch {
	case user != nil && *user && skills > 0:
		add(fmt.Sprintf("your skills +%d", skills))
	case user != nil && *user:
		add("your skills")
	case user != nil:
		add("not your skills")
	}
	if skills > 0 && (user == nil || !*user) {
		add(count(skills, "skill dir", "skill dirs"))
	}
	if n := len(o.Opts[OptInclude].Items); n > 0 {
		add("+" + count(n, "source", "sources"))
	}
	// The primary is where a run goes unless somebody said otherwise, which is
	// the only case worth a word.
	if server := o.Server(); server != nil && server.Changed() {
		add("on " + server.Selected())
	}
	return chips
}

// count is n of a thing, singular or plural.
func count(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// Command renders the run invocation the current options describe. It is the
// options' own documentation: what is on screen is reproducible from a shell,
// and if it is not, the options offer something the command cannot.
func (o *RunOptions) Command(prompt string) string {
	return o.CommandFor(o.Request(prompt, o.Opts[OptSource].Selected()))
}

// CommandFor is that invocation for a request already made: the create card
// previews one whose source is the row under its cursor rather than the one
// the Source row is set to.
//
// It spells the run as `discobox run`, the only command that makes one, with
// the prompt as -p: the composer holds one piece of text and sends it as one
// argument, which is exactly what -p is. Trailing words would be inventing a
// tokenization the user did not type.
func (o *RunOptions) CommandFor(req RunRequest) string {
	if o.GitPolicyLoading {
		return "Reading Git credential preference…"
	}
	if o.GitPolicyError {
		return "Cannot read Git credential preference; fix client.yaml before creating"
	}
	args := []string{"discobox"}
	if req.Server != "" {
		args = append(args, "--server", req.Server)
	}
	// --project names a project on the primary, and a run on another server
	// goes to that server's default (ADR 0116 §4): naming this one would offer
	// a line that fails where it was told to run.
	if p := o.Session.Project; p != "" && p != o.Session.DefaultProject && req.Server == "" {
		args = append(args, "--project", p)
	}
	// run's own arguments, after the command name; the ones above are global
	// and go in front of it. SOURCE leads, as `run SOURCE` reads, and is "."
	// for the window's own directory. --no-source takes its place.
	run := []string{"run"}
	if p := o.Session.GitCredentials; p != "" {
		run = append(run, "--git-credentials="+p)
	}
	switch {
	case req.NoSource:
		run = append(run, "--no-source")
	case req.Source != "":
		run = append(run, ShellQuote(req.Source))
	default:
		run = append(run, ".")
	}
	for _, word := range req.Prompt {
		run = append(run, "-p", ShellQuote(word))
	}
	if req.Harness != "" {
		run = append(run, "--harness", req.Harness)
	}
	if req.Pool != "" {
		run = append(run, "--pool", ShellQuote(req.Pool))
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
		run = append(run, "-e", ShellQuote(e))
	}
	for _, s := range req.Secret {
		run = append(run, "-s", ShellQuote(s))
	}
	for _, i := range req.Include {
		run = append(run, "-i", ShellQuote(i))
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
		run = append(run, "--skills", ShellQuote(dir))
	}
	return strings.Join(append(args, run...), " ")
}

// ShellQuote quotes an argument the way the command preview needs it: only
// when the argument would otherwise be split or interpreted.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return strings.ContainsRune(" \t\n\"'\\$`*?![]{}()<>|&;#~", r)
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// UserSkillsConfigured is the User skills row left to the client's
// configuration, which the request carries as no answer at all.
const UserSkillsConfigured = "as configured"

// UserSkills is the User skills row as RunRequest.UserSkills: nil while it is
// left to the configuration.
func (o *RunOptions) UserSkills() *bool {
	switch o.Opts[OptUserSkills].Index {
	case 1:
		return new(true)
	case 2:
		return new(false)
	}
	return nil
}
