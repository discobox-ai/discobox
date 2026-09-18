// Package sandboxmeta is the shape of a sandbox's meta — its description and
// its tags: the file inside the sandbox that holds them, the rules each must
// satisfy, and the selector grammar a listing filters tags by.
//
// A sandbox's meta lives in the sandbox, in a JSON file at RelativePath under
// the sandbox user's home. That file is the system of record: the agent in the
// sandbox edits it directly, an API write is carried into the sandbox and
// applied to it, and the control plane only keeps a copy of what the sandbox
// last reported, for reading a stopped sandbox and for filtering a listing
// (ADR 0136).
//
// It lives in the root module because three modules apply the same rules: the
// sandbox agent reads and writes the file, the server validates what it caches
// and matches listings against it, and the CLI parses the selectors a user
// types. A rule written three times is three rules.
package sandboxmeta

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// RelativePath is where a sandbox's meta lives, relative to the sandbox user's
// home directory. It sits beside the desktop's feedback directory under
// ~/.discobox, the directory in the home that is Discobox's to name.
const RelativePath = ".discobox/meta.json"

// Limits on meta. They are generous for what a person writes and small enough
// that the whole of it rides every status report without anyone noticing.
const (
	MaxDescriptionLength = 4096
	MaxTags              = 64
	MaxKeyLength         = 128
	MaxValueLength       = 256
	// MaxFileSize bounds what is read of the file. Meta at every other limit
	// is well inside it; a file past it is not meta.
	MaxFileSize = 64 << 10
)

// Meta is what the file holds.
type Meta struct {
	// Description says what the sandbox is for, in the words of whoever
	// wrote it. It may run to several lines; a listing shows the first.
	Description string `json:"description,omitempty"`
	// Tags label the sandbox, key to value. A tag with an empty value is a
	// plain label.
	Tags map[string]string `json:"tags,omitempty"`
}

// Parse reads the file's contents. An empty file, or one holding only
// whitespace, is no meta — the same answer as no file at all. Anything else
// must be a JSON object with no fields but description and tags, and all of it
// must be valid: a file with one bad tag is refused whole rather than read in
// part, because a partial read would report as removed a tag the file still
// says is there.
func Parse(data []byte) (Meta, error) {
	if len(data) > MaxFileSize {
		return Meta{}, fmt.Errorf("meta file is larger than %d bytes", MaxFileSize)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return Meta{Tags: map[string]string{}}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var meta Meta
	if err := decoder.Decode(&meta); err != nil {
		return Meta{}, fmt.Errorf(`meta file must be a JSON object like {"description": "...", "tags": {"wip": "", "ticket": "ENG-12"}}: %w`, err)
	}
	if decoder.More() {
		return Meta{}, errors.New("meta file holds more than one JSON value")
	}
	if meta.Tags == nil {
		meta.Tags = map[string]string{}
	}
	if err := meta.Validate(); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// Encode is the file form of meta: indented, tags in key order, and ending in
// a newline, so it reads well and diffs cleanly for whoever edits it next.
func Encode(meta Meta) ([]byte, error) {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Validate checks all of the meta.
func (m Meta) Validate() error {
	if err := ValidateDescription(m.Description); err != nil {
		return err
	}
	return ValidateTags(m.Tags)
}

// ValidateDescription checks a description. Line breaks and tabs are allowed;
// other control characters are not, since a listing draws it as text.
func ValidateDescription(description string) error {
	switch {
	case !utf8.ValidString(description):
		return errors.New("the description is not valid UTF-8")
	case len(description) > MaxDescriptionLength:
		return fmt.Errorf("the description is longer than %d bytes", MaxDescriptionLength)
	}
	for _, r := range description {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("the description cannot contain control characters other than line breaks and tabs")
		}
	}
	return nil
}

// ValidateTags checks a whole tag set.
func ValidateTags(tags map[string]string) error {
	if len(tags) > MaxTags {
		return fmt.Errorf("a sandbox holds at most %d tags, not %d", MaxTags, len(tags))
	}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		if err := ValidateKey(key); err != nil {
			return err
		}
		if err := ValidateValue(key, tags[key]); err != nil {
			return err
		}
	}
	return nil
}

// ValidateKey checks one tag key. A key is printable, has no whitespace, and
// uses neither `=` nor `,`: those two are what the selector grammar and the
// listing separate tags with, and a key containing either could not be
// filtered on or read back off a listing.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("a tag key cannot be empty")
	case !utf8.ValidString(key):
		return fmt.Errorf("tag key %q is not valid UTF-8", key)
	case len(key) > MaxKeyLength:
		return fmt.Errorf("tag key %q is longer than %d bytes", key, MaxKeyLength)
	}
	for _, r := range key {
		switch {
		case r == '=' || r == ',':
			return fmt.Errorf("tag key %q cannot contain %q", key, r)
		case unicode.IsSpace(r) || !unicode.IsPrint(r):
			return fmt.Errorf("tag key %q cannot contain whitespace or control characters", key)
		}
	}
	return nil
}

// ValidateValue checks one tag value. A value may be empty — a tag with no
// value is a plain label — and may contain spaces and `=`, but not `,`, which
// is what a listing separates tags with, nor line breaks or other control
// characters, since a listing draws it on one line.
func ValidateValue(key, value string) error {
	switch {
	case !utf8.ValidString(value):
		return fmt.Errorf("the value of tag %q is not valid UTF-8", key)
	case len(value) > MaxValueLength:
		return fmt.Errorf("the value of tag %q is longer than %d bytes", key, MaxValueLength)
	}
	for _, r := range value {
		switch {
		case r == ',':
			return fmt.Errorf("the value of tag %q cannot contain ','", key)
		case unicode.IsControl(r):
			return fmt.Errorf("the value of tag %q cannot contain control characters", key)
		}
	}
	return nil
}

// Change is an edit to meta. Whatever it does not name is left as it is,
// including what the sandbox wrote itself.
type Change struct {
	// Description, when set, replaces the description; an empty one clears it.
	Description *string
	// SetTags adds or overwrites tags.
	SetTags map[string]string
	// RemoveTags deletes tags. Naming one the meta does not have is not an
	// error; naming one SetTags also names is, because the change does not say
	// which it means.
	RemoveTags []string
}

// Check validates the change on its own, as far as it can be without the meta
// it will apply to.
func (c Change) Check() error {
	_, err := Apply(Meta{}, c)
	return err
}

// Apply returns meta with the change made, and checks the result. meta itself
// is not modified.
func Apply(meta Meta, change Change) (Meta, error) {
	out := Meta{Description: meta.Description, Tags: make(map[string]string, len(meta.Tags)+len(change.SetTags))}
	maps.Copy(out.Tags, meta.Tags)
	if change.Description != nil {
		out.Description = *change.Description
	}
	for _, key := range change.RemoveTags {
		if err := ValidateKey(key); err != nil {
			return Meta{}, err
		}
		if _, ok := change.SetTags[key]; ok {
			return Meta{}, fmt.Errorf("tag %q is both set and removed", key)
		}
		delete(out.Tags, key)
	}
	maps.Copy(out.Tags, change.SetTags)
	if err := out.Validate(); err != nil {
		return Meta{}, err
	}
	return out, nil
}

// TagStrings is a tag set as a listing spells it: `key=value`, or `key` alone
// for a tag with no value, in key order.
func TagStrings(tags map[string]string) []string {
	out := make([]string, 0, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		if value := tags[key]; value != "" {
			out = append(out, key+"="+value)
		} else {
			out = append(out, key)
		}
	}
	return out
}

// FormatTags is TagStrings joined with commas: the form one listing column
// holds.
func FormatTags(tags map[string]string) string {
	return strings.Join(TagStrings(tags), ",")
}

// Summary is the description as a listing shows it: its first non-blank line.
func Summary(description string) string {
	for line := range strings.SplitSeq(description, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// Selector matches a tag set: `key` matches a set that has the key, whatever
// its value, and `key=value` one where the key has exactly that value.
type Selector struct {
	Key   string
	Value string
	// HasValue distinguishes `key=` — the key with an empty value, which is a
	// plain label — from `key`, which matches any value.
	HasValue bool
}

// ParseSelector reads `key` or `key=value`.
func ParseSelector(text string) (Selector, error) {
	key, value, hasValue := strings.Cut(strings.TrimSpace(text), "=")
	if err := ValidateKey(key); err != nil {
		return Selector{}, fmt.Errorf("tag selector %q: %w", text, err)
	}
	if err := ValidateValue(key, value); err != nil {
		return Selector{}, fmt.Errorf("tag selector %q: %w", text, err)
	}
	return Selector{Key: key, Value: value, HasValue: hasValue}, nil
}

// ParseSelectors reads every selector in texts.
func ParseSelectors(texts []string) ([]Selector, error) {
	out := make([]Selector, 0, len(texts))
	for _, text := range texts {
		selector, err := ParseSelector(text)
		if err != nil {
			return nil, err
		}
		out = append(out, selector)
	}
	return out, nil
}

// String is the selector as ParseSelector reads it.
func (s Selector) String() string {
	if s.HasValue {
		return s.Key + "=" + s.Value
	}
	return s.Key
}

// Matches reports whether tags satisfies the selector.
func (s Selector) Matches(tags map[string]string) bool {
	value, ok := tags[s.Key]
	return ok && (!s.HasValue || value == s.Value)
}

// MatchesAll reports whether tags satisfies every selector. No selectors
// match everything.
func MatchesAll(tags map[string]string, selectors []Selector) bool {
	for _, selector := range selectors {
		if !selector.Matches(tags) {
			return false
		}
	}
	return true
}
