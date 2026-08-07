package ui

import (
	"strings"
)

// runOptions is every flag `disco run` takes, laid out as one editable list.
//
// The panel is a picker, not a form: left and right change a value in place,
// so the common case — swap the harness, turn dirty-carry off — never opens a
// text field. Only the free text options (name, env, secrets) do.
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
		return o.choices[o.idx]
	case optToggle:
		if o.value == "on" {
			return "yes"
		}
		return "no"
	case optText:
		if strings.TrimSpace(o.value) == "" {
			return "(auto)"
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
		o.idx = (o.idx + delta + len(o.choices)) % len(o.choices)
	case optToggle:
		if o.value == "on" {
			o.value = "off"
		} else {
			o.value = "on"
		}
	}
}

// optionSet is the panel: the options plus its own cursor. The project comes
// from the session rather than the panel, and is carried here only so the
// command preview can show the flag it would need.
type optionSet struct {
	opts    []*option
	cursor  int
	project string
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

func newOptions(project string) *optionSet {
	return &optionSet{project: project, opts: []*option{
		optHarness: {
			label: "Harness", kind: optChoice,
			choices: []string{"claude", "codex", "cursor", "none (shell)"},
			hint:    "--harness · empty is the project default, which is claude",
		},
		optDirty: {
			label: "Uncommitted changes", kind: optChoice,
			choices: []string{"auto", "include", "exclude"},
			hint:    "--include-dirty · auto asks when the working tree is dirty",
		},
		optDetach: {
			label: "Detach", kind: optToggle, value: "off",
			hint: "-d · create the sandbox and print it, without attaching",
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
			choices: []string{
				currentDir + " @ " + currentBranch,
				currentDir + " @ HEAD~1",
				currentDir + " @ origin/main",
			},
			hint: "-C · the directory and ref the sandbox is cut from",
		},
	}}
}

func (o *optionSet) current() *option { return o.opts[o.cursor] }

func (o *optionSet) move(delta int) {
	o.cursor = (o.cursor + delta + len(o.opts)) % len(o.opts)
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
	if harness.idx == 3 {
		add(true, "no harness")
	} else {
		add(true, harness.display())
	}

	switch o.opts[optDirty].idx {
	case 1:
		add(true, "+dirty")
	case 2:
		add(true, "clean")
	default:
		add(false, "dirty auto")
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

	add(o.opts[optSource].changed(), o.opts[optSource].display())

	return st.chipOn.Render("⏵⏵ ") + strings.Join(parts, st.chip.Render(" · "))
}

// command renders the `disco run` invocation the current options describe.
func (o *optionSet) command(prompt string) string {
	args := []string{"disco"}
	if o.project != "" && o.project != defaultProject {
		args = append(args, "-p", o.project)
	}
	if src := o.opts[optSource]; src.changed() {
		args = append(args, "-C", shellQuote(strings.ReplaceAll(src.display(), " @ ", "@")))
	}
	args = append(args, "run")
	if h := o.opts[optHarness]; h.changed() {
		if h.idx == 3 {
			args = append(args, "--harness", "''")
		} else {
			args = append(args, "--harness", h.choices[h.idx])
		}
	}
	switch o.opts[optDirty].idx {
	case 1:
		args = append(args, "--include-dirty=true")
	case 2:
		args = append(args, "--include-dirty=false")
	}
	if o.opts[optDetach].changed() {
		args = append(args, "-d")
	}
	for _, e := range o.opts[optEnv].items {
		args = append(args, "-e", shellQuote(e))
	}
	for _, s := range o.opts[optSecret].items {
		args = append(args, "-s", shellQuote(s))
	}
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		args = append(args, "--", shellQuote(prompt))
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
		b.WriteString(padANSI(row, inner) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(st.dimText.Render(truncate(o.current().hint, inner)))
	b.WriteString("\n\n")
	b.WriteString(st.dimText.Render("would run"))
	b.WriteString("\n")
	for _, l := range wrap(o.command(prompt), inner) {
		b.WriteString(st.command.Render(l) + "\n")
	}

	return st.dialog.Width(boxWidth).Render(b.String())
}
