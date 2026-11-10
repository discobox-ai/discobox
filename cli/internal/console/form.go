package console

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Forms (ADR 26-11-07-876 §1): every question the console asks as a card, said
// once here as a description that any renderer draws. A form is its fields
// (what each asks, its choices and what it opens on, when it applies), and its
// submit, which checks the answers and turns them into the request they make.
// A renderer keeps what is being typed and where the cursor is; it never
// decides whether an answer is acceptable or what it means.

// Choice is one option of a picker field.
type Choice struct {
	Key   string
	Label string
	// Hint is what choosing it means, said under the card while it is chosen.
	Hint string
}

// Field is one line of a form: a label, and a value either typed or chosen.
// Choices make it a picker; without them it takes text.
type Field struct {
	Key   string
	Label string
	// Section is the rule the field sits under. A field repeating the section
	// of the one above it is under the same rule.
	Section string
	// Hint is why the field is asked, said under the card while it is being
	// answered. A picker's chosen option says its own, when it has one.
	Hint string

	Choices []Choice
	// Value is what the field opens on: the text of a typed field, or the key
	// of a picker's option. A picker whose Value names none of its options
	// opens on the first.
	Value string
	// Placeholder is what an empty typed field says it wants.
	Placeholder string
	// Suffix is the unit a typed value is in: "seconds".
	Suffix string
	// Masked hides what is typed, for a field that takes a credential.
	Masked bool

	// When decides whether the field applies, from the answers so far. Nil
	// always applies. A field that does not apply is still shown, with Why in
	// place of its value: a form is the whole decision on one card, and taking
	// a field away would make an answer above it produce fields nobody knew
	// were coming.
	When func(Answers) bool
	// Locked is a field this form never answers: what a thing already is,
	// shown in the place the form would have asked it.
	Locked bool
	// Why stands in for the value of a field that cannot be answered.
	Why string
	// Required refuses an empty typed field that applies, saying why.
	Required string
}

// Picker is whether the field chooses between options rather than taking text.
func (f Field) Picker() bool { return len(f.Choices) > 0 }

// Applies is whether the field takes an answer, given the others.
func (f Field) Applies(a Answers) bool {
	return !f.Locked && (f.When == nil || f.When(a))
}

// Answers are a form's answers by field key: a typed field's text, and the key
// of a picker's chosen option. Every field has one, whether it applies or not.
type Answers map[string]string

// Get is one answer, trimmed: nothing typed in a form means anything by its
// surrounding space.
func (a Answers) Get(key string) string { return strings.TrimSpace(a[key]) }

// Refusal is why a form's answers were not accepted, and the field it is
// about, when it is about one. The form stays up holding what was typed.
type Refusal struct {
	Field string
	Why   string
}

func (r *Refusal) Error() string { return r.Why }

// refuse is a refusal about the answers as a whole.
func refuse(why string) *Refusal { return &Refusal{Why: why} }

// Form is a question with the answer T: its fields, and the submit that turns
// answers into T or refuses them.
type Form[T any] struct {
	// Fields are the fields as the form opens.
	Fields []Field
	// Grow is the fields for the answers so far, for a form whose repeated
	// rows grow a field as their last is filled (the run options' env and
	// secrets). Nil is a form whose fields are Fields whatever is answered.
	Grow func(Answers) []Field
	// Note is what the answers so far add up to, said under the fields (the
	// command a create would run). Nil says nothing.
	Note func(Answers) string
	// Submit reads the answers once every required field holds one. Nil
	// answers the zero T.
	Submit func(Answers) (T, *Refusal)
}

// FieldsFor is the form's fields for the answers so far (Grow).
func (f Form[T]) FieldsFor(a Answers) []Field {
	if f.Grow == nil {
		return f.Fields
	}
	return f.Grow(a)
}

// NoteFor is what the answers so far add up to (Note), or nothing.
func (f Form[T]) NoteFor(a Answers) string {
	if f.Note == nil {
		return ""
	}
	return f.Note(a)
}

// Answer checks the answers and makes them T: a required field that applies
// and is empty is refused first, then whatever Submit refuses.
func (f Form[T]) Answer(a Answers) (T, *Refusal) {
	var zero T
	for _, field := range f.FieldsFor(a) {
		if field.Required == "" || field.Picker() || !field.Applies(a) {
			continue
		}
		if a.Get(field.Key) == "" {
			return zero, &Refusal{Field: field.Key, Why: field.Required}
		}
	}
	if f.Submit == nil {
		return zero, nil
	}
	return f.Submit(a)
}

// Opening is the answers as the form opens, before anything is typed or
// chosen.
func (f Form[T]) Opening() Answers { return opening(f.Fields) }

// opening is what fields hold as their form opens. A form that answers only
// what changed compares each answer with this, its one measure of "untouched".
func opening(fields []Field) Answers {
	a := Answers{}
	for _, field := range fields {
		a[field.Key] = field.Opens()
	}
	return a
}

// Opens is what the field holds as its form opens: its typed value as one
// line, or the key of the option it opens on.
func (f Field) Opens() string {
	if !f.Picker() {
		return OneLine(f.Value)
	}
	for _, c := range f.Choices {
		if c.Key == f.Value {
			return c.Key
		}
	}
	return f.Choices[0].Key
}

// OneLine is text as a typed field holds it: one line, each tab, carriage
// return and line feed a space, and every other control character and every
// rune that does not decode (or decodes to U+FFFD) dropped — rune for rune what
// the TUI's field (bubbles' textinput) makes of a value set on it. A form that
// answers only what changed compares with what the field opened holding, which
// is this; if the two disagreed, an untouched field would read as an edit.
func OneLine(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteByte(' ')
		case unicode.IsControl(r):
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
