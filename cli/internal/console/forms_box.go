package console

import (
	"slices"
	"strings"
	"unicode"
)

// The forms about one box and what it is created from: rename, settings, the
// fields the create card asks a source and a repeated option's value in, and
// a harness variant.

// Rename is an answered rename: the name to give, or Unchanged when the answer
// asks for nothing.
type Rename struct {
	Name      string
	Unchanged bool
}

// RenameForm asks what a box should be called, opening on title, the name it
// reads as now (its given name, else the title its harness set), so the
// common edit — a word added to a name that is nearly right — is a word typed
// rather than a name retyped.
func RenameForm(box Sandbox, title string) Form[Rename] {
	question := "What should this discobox be called?"
	if box.GivenName != "" {
		question += " An empty name gives back the title its harness set."
	}
	fields := []Field{{Key: "name", Label: "name", Hint: question, Placeholder: "name", Value: title}}
	opened := opening(fields)
	return Form[Rename]{
		Fields: fields,
		Submit: func(a Answers) (Rename, *Refusal) {
			// Enter on what is shown asks for nothing, whichever name that is:
			// on a box nobody has named it is the title its harness set, and
			// accepting it would freeze that as the box's name. An empty line
			// takes a given name away, which puts the title back; on a box with
			// none there is nothing to take.
			name := a.Get("name")
			if name == opened.Get("name") || (name == "" && box.GivenName == "") {
				return Rename{Unchanged: true}, nil
			}
			return Rename{Name: name}, nil
		},
	}
}

// SettingsForm is `discobox admin box update` as one form: the configured
// name, the harness, the box's own egress level and its discobox API access,
// opened on what is stored. Its answer is only the parts that changed, so a
// save never writes back a value somebody else changed meanwhile that the form
// did not touch; an empty update is nothing to save.
//
// The harness field applies only to a stopped box, because the server refuses
// a harness change on anything else and the form should say so before the
// save does.
func SettingsForm(box Sandbox, was BoxSettings, harnesses []Harness) Form[BoxSettingsUpdate] {
	stopped := box.State == StateStopped
	yesNo := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	const egress, access = "Egress", "Access"
	fields := []Field{
		{Key: "name", Label: "name", Placeholder: "the name it is configured with", Value: was.Name,
			Required: "a discobox needs a configured name",
			Hint:     "the handle it was created with, which commands take · the list shows the name rename gives it, or its agent's title"},
		{Key: "harness", Label: "harness", Choices: HarnessChoices(was.HarnessID, box, harnesses), Value: was.HarnessID,
			Hint: "the new harness provisions on the next start · conversation state is not carried over",
			When: func(Answers) bool { return stopped }, Why: "stop the box to change its harness"},
		{Key: "deny", Label: "restrict", Section: egress, Value: yesNo(was.Egress.DenyByDefault), Choices: []Choice{
			{Key: "no", Label: "no", Hint: "this level restricts nothing · the project, pool or harness may still"},
			{Key: "yes", Label: "deny by default", Hint: "reach only every level's allow entries and the hosts its harness image declares"}}},
		{Key: "allow", Label: "allow", Section: egress, Placeholder: "hosts, *.wildcards, IPs or CIDRs, comma-separated",
			Value: strings.Join(was.Egress.Allow, ", "),
			Hint:  "added to the allow list of a restricted box · an allow list alone restricts nothing · empty is none"},
		{Key: "passthrough", Label: "passthrough", Section: egress, Placeholder: "hosts whose HTTPS is passed through unread",
			Value: strings.Join(was.Egress.Passthrough, ", "),
			Hint:  "spliced to the origin unread, TLS end to end: no credential is swapped in · it admits no host"},
		{Key: "api", Label: "discobox API", Section: access, Value: yesNo(was.DiscoboxAPI), Choices: []Choice{
			{Key: "no", Label: "no", Hint: "the box cannot call the discobox API"},
			{Key: "yes", Label: "yes", Hint: "the box may call the discobox API through its pool, acting only on the discoboxes it leads"}}},
	}
	// The name is compared with what its field opens holding (opening), not
	// with what was read: a field holds one line, and a stored name can carry
	// a character it drops or folds — a save of anything else would then
	// rename the box as well.
	opened := opening(fields)
	return Form[BoxSettingsUpdate]{Fields: fields, Submit: func(a Answers) (BoxSettingsUpdate, *Refusal) {
		var update BoxSettingsUpdate
		if name := a.Get("name"); name != opened.Get("name") {
			update.Name = &name
		}
		if harness := a.Get("harness"); stopped && harness != "" && harness != was.HarnessID {
			update.Harness = &harness
		}
		if deny := a.Get("deny") == "yes"; deny != was.Egress.DenyByDefault {
			update.EgressDeny = &deny
		}
		if allow := HostList(a.Get("allow")); !slices.Equal(allow, HostList(opened.Get("allow"))) {
			update.EgressAllow = &allow
		}
		if passthrough := HostList(a.Get("passthrough")); !slices.Equal(passthrough, HostList(opened.Get("passthrough"))) {
			update.EgressPassthrough = &passthrough
		}
		if api := a.Get("api") == "yes"; api != was.DiscoboxAPI {
			update.DiscoboxAPI = &api
		}
		return update, nil
	}}
}

// HarnessChoices is every harness on the box's server, led by the one it has
// so the field opens on it.
func HarnessChoices(current string, box Sandbox, harnesses []Harness) []Choice {
	var choices []Choice
	found := false
	for _, h := range harnesses {
		found = found || h.ID == current
		hint := h.Name
		if h.ID == current {
			hint = "what the box has now"
		}
		choices = append(choices, Choice{Key: h.ID, Label: h.FlagName(), Hint: hint})
	}
	if !found {
		// A harness the listing does not hold — deleted, or the listing
		// failed — is still the box's, and the field has to open on it.
		label := box.Harness
		if label == "" {
			label = current
		}
		choices = append([]Choice{{Key: current, Label: label, Hint: "what the box has now"}}, choices...)
	}
	for i, c := range choices {
		if c.Key == current {
			choices = append(append([]Choice{c}, choices[:i]...), choices[i+1:]...)
			break
		}
	}
	return choices
}

// HostList is a typed list of egress entries: separated by commas or spaces,
// since no entry holds either, with the empty ones a trailing comma leaves
// dropped. Nothing typed is the empty list, never nil, so it compares equal to
// a level that stores none.
func HostList(text string) []string {
	return append([]string{}, strings.FieldsFunc(text, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })...)
}

// SourceForm is the field a source the listing has never seen is typed into:
// a directory, a URL or owner/repo. ask is what it says it wants (or what was
// wrong with the last thing typed), and value what it opens holding. Its
// answer is the source as typed, empty for the folder its placeholder offers;
// whether that is a source at all is DataSource.ResolveSource's to say.
func SourceForm(ask, placeholder, value string) Form[string] {
	return Form[string]{
		Fields: []Field{{Key: "source", Label: "source", Hint: ask, Placeholder: placeholder, Value: value}},
		Submit: func(a Answers) (string, *Refusal) { return a.Get("source"), nil },
	}
}

// ItemForm is the field one more value of a repeated run option is typed
// into: an env, a secret, a skills directory, another source. Its answer is
// the value, empty for none.
func ItemForm(ask, example, value string) Form[string] {
	return Form[string]{
		Fields: []Field{{Key: "item", Label: "value", Hint: ask, Placeholder: example, Value: value}},
		Submit: func(a Answers) (string, *Refusal) { return a.Get("item"), nil },
	}
}

// HarnessVariantForm is a variant of parent: its name and slug, both required.
// The parent is shown, locked.
func HarnessVariantForm(parent Harness) Form[NewHarnessVariant] {
	return Form[NewHarnessVariant]{
		Fields: []Field{
			{Key: "parent", Label: "Parent", Value: parent.DisplayName(), Locked: true, Why: parent.DisplayName()},
			{Key: "name", Label: "Name", Placeholder: parent.Name + " (work)", Required: "give the variant a name"},
			{Key: "slug", Label: "Slug", Placeholder: parent.Slug + "-work", Required: "give the variant a slug"},
		},
		Submit: func(a Answers) (NewHarnessVariant, *Refusal) {
			return NewHarnessVariant{ParentID: parent.ID, Name: a.Get("name"), Slug: a.Get("slug")}, nil
		},
	}
}
