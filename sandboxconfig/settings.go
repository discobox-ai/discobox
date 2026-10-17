package sandboxconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
)

// A repository's settings are one file at its root (ADR 26-10-16-423 §11).
// Each reader takes only its own keys: the client `sources`, before the box
// exists, and the pool agent the project layer, out of the materialized clone.
//
// The two files it replaces live in users' repositories and cannot be
// migrated, so they are read in its place when it is absent — sources.json
// by the client and project.json by the pool agent — and a repository holding
// both kinds is refused rather than merged.
const (
	SettingsFile      = ".discobox/settings.json"
	LegacySourcesFile = ".discobox/sources.json"
	LegacyProjectFile = ".discobox/project.json"
)

// SettingsSchemaID is where the generated schema is published, and what a
// settings file's `$schema` names.
const SettingsSchemaID = "https://raw.githubusercontent.com/discobox-ai/discobox/main/settings.schema.json"

// ErrSettingsConflict is a repository holding SettingsFile beside a file it
// replaces.
var ErrSettingsConflict = errors.New("conflicting repository settings")

// Settings is .discobox/settings.json.
type Settings struct {
	Schema string `json:"$schema,omitempty" doc:"The JSON Schema this file is checked against, for an editor."`
	// Sources is read by the client, which resolves each one against its own
	// disk before the box exists (ADR 0056).
	Sources map[string]string `json:"sources,omitempty" doc:"The other repositories this one is worked on with, by name: a Git URL with an optional @REF. The checkout beside this repository named the same is used when there is one, and the URL is cloned to where it would have been when there is not." example:"{x: https://github.com/discobox-ai/x}"`
	// Features opts a box's features out (ADR 26-10-16-423 §4). It is
	// decoded and checked, and nothing acts on it yet: no feature is
	// provisioned by name.
	Features map[string]Feature `json:"features,omitempty" doc:"The box's features by name, each true, false, or an object whose enabled is false to turn it off. Checked, but no feature reads it yet." example:"{brew: false}"`
	ProjectLayer
}

// Feature is one entry of Settings.Features: true or false, or an object, so
// a value can carry a feature's settings later without a schema change. An
// object without enabled leaves the feature on.
type Feature struct {
	Enabled bool `json:"enabled"`
}

// UnmarshalJSON reads a feature written as a boolean or as an object.
func (f *Feature) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case bytes.Equal(data, []byte("true")), bytes.Equal(data, []byte("false")):
		f.Enabled = data[0] == 't'
		return nil
	case len(data) > 0 && data[0] == '{':
		var object struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.Unmarshal(data, &object); err != nil {
			return err
		}
		f.Enabled = object.Enabled == nil || *object.Enabled
		return nil
	}
	return fmt.Errorf("a feature is true, false or an object, not %q", data)
}

// ConfigSchema is a feature's JSON Schema, which a struct cannot spell
// (configfile.Schemer).
func (Feature) ConfigSchema() map[string]any {
	return map[string]any{
		"anyOf": []any{
			map[string]any{"type": "boolean"},
			map[string]any{
				"type": "object",
				// Open, so a feature's settings can be added without a
				// schema change.
				"additionalProperties": true,
				"properties": map[string]any{
					"enabled": map[string]any{
						"type":        "boolean",
						"description": "False turns the feature off. Absent leaves it on.",
					},
				},
			},
		},
	}
}

// RepositorySettings is what a repository holds of its settings.
type RepositorySettings struct {
	// Path is the file read: SettingsFile, the legacy file the reader asked
	// for in its place, or empty when the repository has neither.
	Path string
	// Data is that file's content.
	Data []byte
	// Legacy names the legacy files the repository holds without a
	// SettingsFile, which a reader warns about.
	Legacy []string
}

// ReadSettings reads a repository's settings for the reader whose keys
// legacy held, LegacySourcesFile or LegacyProjectFile. read answers a file by
// its path from the repository's root, and an error matching fs.ErrNotExist
// for one the repository does not hold.
func ReadSettings(read func(path string) ([]byte, error), legacy string) (RepositorySettings, error) {
	held := map[string][]byte{}
	for _, path := range []string{SettingsFile, LegacySourcesFile, LegacyProjectFile} {
		data, err := read(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return RepositorySettings{}, fmt.Errorf("read %s: %w", path, err)
		}
		held[path] = data
	}
	var out RepositorySettings
	for _, path := range []string{LegacySourcesFile, LegacyProjectFile} {
		if _, ok := held[path]; ok {
			out.Legacy = append(out.Legacy, path)
		}
	}
	if data, ok := held[SettingsFile]; ok {
		switch len(out.Legacy) {
		case 1:
			return RepositorySettings{}, fmt.Errorf("%w: %s replaces %s; move what it holds into %s and delete it",
				ErrSettingsConflict, SettingsFile, out.Legacy[0], SettingsFile)
		case 2:
			return RepositorySettings{}, fmt.Errorf("%w: %s replaces %s and %s; move what they hold into %s and delete them",
				ErrSettingsConflict, SettingsFile, out.Legacy[0], out.Legacy[1], SettingsFile)
		}
		out.Path, out.Data = SettingsFile, data
		return out, nil
	}
	if data, ok := held[legacy]; ok {
		out.Path, out.Data = legacy, data
	}
	return out, nil
}

// Settings decodes the file read, for every reader. A legacy file fills the
// keys it held: sources.json is the sources object itself, and project.json the
// project layer.
//
// A key Settings does not define is left to UnknownKeys rather than refused:
// the file lives in a repository read by every client and sandbox agent its
// people run, and a key a newer one added must not stop an older one.
func (r RepositorySettings) Settings() (Settings, error) {
	var out Settings
	var err error
	switch r.Path {
	case "":
		return out, nil
	case LegacySourcesFile:
		err = json.Unmarshal(r.Data, &out.Sources)
	case LegacyProjectFile:
		err = json.Unmarshal(r.Data, &out.ProjectLayer)
	default:
		err = json.Unmarshal(r.Data, &out)
	}
	if err != nil {
		return Settings{}, fmt.Errorf("parse %s: %w", r.Path, err)
	}
	return out, nil
}

// UnknownKeys are the keys of a settings.json that Settings does not define,
// with their values: misspelled, or added by a newer Discobox. The
// client warns about them; nothing refuses a file for holding them.
func (r RepositorySettings) UnknownKeys() map[string]json.RawMessage {
	if r.Path != SettingsFile {
		return nil
	}
	var keys map[string]json.RawMessage
	if json.Unmarshal(r.Data, &keys) != nil {
		return nil
	}
	known := settingsKeys()
	for key := range keys {
		if slices.ContainsFunc(known, func(k string) bool { return strings.EqualFold(k, key) }) {
			delete(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// settingsKeys are the keys Settings defines, in the order it declares them.
func settingsKeys() []string {
	var keys []string
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		for i := range t.NumField() {
			field := t.Field(i)
			if field.Anonymous {
				walk(field.Type)
				continue
			}
			if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" && name != "-" {
				keys = append(keys, name)
			}
		}
	}
	walk(reflect.TypeOf(Settings{}))
	return keys
}

// LegacyWarning says what a reader warns about a repository read through its
// legacy files, or nothing when it has none.
func (r RepositorySettings) LegacyWarning() string {
	switch len(r.Legacy) {
	case 0:
		return ""
	case 1:
		return fmt.Sprintf("%s is read because the repository has no %s; move what it holds into %s", r.Legacy[0], SettingsFile, SettingsFile)
	default:
		return fmt.Sprintf("%s and %s are read because the repository has no %s; move what they hold into %s", r.Legacy[0], r.Legacy[1], SettingsFile, SettingsFile)
	}
}
