package tui

import (
	"strings"
)

// runOptions is every flag `disco run` takes, laid out as one editable list.
//
// The panel is a picker, not a form: left and right change a value in place,
// so the common case — swap the harness, turn dirty-carry off — never opens a
// text field. Only the free text options (env, secrets, source) do.
type optKind int

const (
	optChoice optKind = iota
	optToggle
	optText
	optMulti // repeated flag: -e, -s
)

type option struct {
	label   string
	kind    optKind
	choices []string
	idx     int
	value   string
	items   []string
	hint    string

	// placeholder is what a text option shows while it is unset: the value the
	// CLI would use anyway, so the panel never reads as "nothing here".
	placeholder string
}

// changed reports whether the option differs from the CLI's own default, which
// is what decides if it shows up in the chip strip and in the command preview.
func (o *option) changed() bool {
	switch o.kind {
	case optChoice:
		return o.idx != 0
	case optToggle:
		return o.value == "on"
	case optText:
		return strings.TrimSpace(o.value) != ""
	case optMulti:
		return len(o.items) > 0
	}
	return false
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
	case optText:
		if strings.TrimSpace(o.value) == "" {
			return o.placeholder
		}
		return o.value
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
}

// Index into optionSet.opts. The command builder reads by name, so the order
// here is only the order they are shown in.
//
// These are `disco run`'s flags and nothing else: --harness, --include-dirty,
// --detach, -e and -s, plus -C, the source the sandbox is cut from. A launcher
// that offers options the command does not have is a launcher you cannot
// reproduce from a shell.
//
// The project is not among them. It is set once for the session, the way
// `disco -p foo` sets it for a shell, so it belongs in the header rather than
// in a panel you would have to open to find out where a sandbox went.
const (
	optHarness = iota
	optDirty
	optDetach
	optEnv
	optSecret
	optSource
)

// noHarness is the choice that runs a shell instead of an agent. It is spelled
// out rather than left as an empty row, because a list with a blank option in
// it reads as a bug.
const noHarness = "none (shell)"

func newOptions(session Session) *optionSet {
	// The project default leads, so the common case is index zero and emits no
	// flag at all — the same shape every other option here has.
	choices := make([]string, 0, len(session.Harnesses)+1)
	if session.DefaultHarness != "" {
		choices = append(choices, session.DefaultHarness)
	}
	for _, h := range session.Harnesses {
		if h != session.DefaultHarness {
			choices = append(choices, h)
		}
	}
	choices = append(choices, noHarness)

	harnessHint := "--harness · the first is the project default"
	if session.DefaultHarness != "" {
		harnessHint = "--harness · empty is the project default, which is " + session.DefaultHarness
	}

	return &optionSet{session: session, opts: []*option{
		optHarness: {
			label: "Harness", kind: optChoice,
			choices: choices,
			hint:    harnessHint,
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
			label: "Source", kind: optText,
			placeholder: session.sourceLabel(),
			hint:        "-C · the directory and ref the discobox is cut from, as DIR@REF",
		},
	}}
}

// setFolder points the run source at the folder the header has moved to. An
// explicit source is cleared with it: the header is where the folder is chosen,
// and leaving a stale override behind would mean the window says one thing and
// creates in another.
func (o *optionSet) setFolder(folder string) {
	o.folder = folder
	o.opts[optSource].value = ""
	o.opts[optSource].placeholder = o.sourceLabel()
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

// request is what Enter actually asks for: the options as `disco run`'s
// arguments, with the prompt from the composer.
func (o *optionSet) request(prompt string) RunRequest {
	req := RunRequest{
		Prompt: strings.TrimSpace(prompt),
		Detach: o.opts[optDetach].changed(),
		Env:    append([]string(nil), o.opts[optEnv].items...),
		Secret: append([]string(nil), o.opts[optSecret].items...),
		Source: strings.TrimSpace(o.opts[optSource].value),
	}
	// With nothing typed the source is the folder the header is on. Naming the
	// session's own directory would only repeat the CLI's default, so that one
	// case stays empty and `disco run` resolves it the way it always does.
	if req.Source == "" && o.sourceDir() != o.session.Directory {
		req.Source = o.sourceDir()
	}
	if h := o.opts[optHarness]; h.changed() {
		req.Harness = h.choices[h.idx]
		if req.Harness == noHarness {
			// An explicitly empty harness is how the CLI spells "no agent, just
			// a shell", and is different from not passing the flag at all.
			req.Harness = ""
			req.NoHarness = true
		}
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
	parts := []string{}
	add := func(changed bool, text string) {
		style := st.chip
		if changed {
			style = st.chipOn
		}
		parts = append(parts, style.Render(text))
	}

	// The harness is what the sandbox will actually be, so it reads as a
	// value rather than as a setting: lit whether or not it was changed.
	harness := o.opts[optHarness]
	if harness.display() == noHarness {
		add(true, "no harness")
	} else if harness.display() != "" {
		add(true, harness.display())
	}

	// Only the answers that were given. Auto is what the option already does,
	// and a strip that names every default is a strip you stop reading for the
	// one thing on it that was chosen.
	switch o.opts[optDirty].idx {
	case 1:
		add(true, "+dirty")
	case 2:
		add(true, "clean")
	}

	// Attaching is what happens unless you say otherwise, and a line that
	// says what was always going to happen is a line you stop reading.
	if o.opts[optDetach].changed() {
		add(true, "detached")
	}

	if n := len(o.opts[optEnv].items); n > 0 {
		add(true, plural(n, "env", "env"))
	}
	if n := len(o.opts[optSecret].items); n > 0 {
		add(true, plural(n, "secret", "secrets"))
	}

	// The source is only worth a chip when it is not where the window says it
	// is. The header already names the folder the window is working in, and a
	// strip that repeats it teaches you to stop reading the strip.
	if source := o.opts[optSource].display(); source != o.sourceLabel() {
		add(true, source)
	}

	return st.chipOn.Render("⏵⏵ ") + strings.Join(parts, st.chip.Render(" · "))
}

// command renders the `disco run` invocation the current options describe. It
// is the panel's own documentation: what is on screen is reproducible from a
// shell, and if it is not, the panel is offering something the command cannot.
func (o *optionSet) command(prompt string) string {
	req := o.request(prompt)
	args := []string{"disco"}
	if p := o.session.Project; p != "" && p != o.session.DefaultProject {
		args = append(args, "-p", p)
	}
	if req.Source != "" {
		args = append(args, "-C", shellQuote(req.Source))
	}
	args = append(args, "run")
	switch {
	case req.NoHarness:
		args = append(args, "--harness", "''")
	case req.Harness != "":
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
	if req.Prompt != "" {
		args = append(args, "--", shellQuote(req.Prompt))
	}
	return strings.Join(args, " ")
}

func (o *optionSet) view(st *styles, width int, prompt string) string {
	boxWidth := min(max(width-4, 30), 84)
	inner := max(boxWidth-6, 20)

	var b strings.Builder
	b.WriteString(st.dialogTitle.Render("Run Options"))
	b.WriteString("\n")
	b.WriteString(st.dimText.Render(truncate("← → change  ·  Enter edits text  ·  Esc back to the prompt", inner)))
	b.WriteString("\n\n")

	labelW := 21
	for i, opt := range o.opts {
		bar := " "
		label := st.dimText.Render(pad(opt.label, labelW))
		if i == o.cursor {
			bar = st.key.Render("❯")
			label = st.cursorName.Render(pad(opt.label, labelW))
		}
		value := opt.display()
		valueStyle := st.dimText
		switch {
		case opt.changed():
			valueStyle = st.chipOn
		case i == o.cursor:
			valueStyle = st.name
		}
		// The cursor row wears its arrows, so the one affordance the panel
		// depends on — left and right change the value — is never a guess.
		if i == o.cursor && (opt.kind == optChoice || opt.kind == optToggle) {
			value = "‹ " + value + " ›"
		}
		row := bar + " " + label + " " + valueStyle.Render(truncate(value, max(inner-labelW-3, 6)))
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
