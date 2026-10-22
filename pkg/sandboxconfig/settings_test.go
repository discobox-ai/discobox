package sandboxconfig

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// repository answers ReadSettings from the files it holds.
func repository(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return []byte(data), nil
	}
}

// settings.json holds what both old files held, and each reader takes its own
// keys from it.
func TestReadSettingsReadsTheSettingsFile(t *testing.T) {
	read := repository(map[string]string{SettingsFile: `{
		"$schema": "` + SettingsSchemaID + `",
		"sources": {"x": "https://github.com/discobox-ai/x"},
		"features": {"brew": false, "nix": {"enabled": true}, "go": {}},
		"runCommand": ["make", "dev"],
		"workingDirectorySubpath": "app",
		"filesAdd": [{"path": ".config/a", "content": "b"}]
	}`})
	for _, legacy := range []string{LegacySourcesFile, LegacyProjectFile} {
		got, err := ReadSettings(read, legacy)
		if err != nil {
			t.Fatalf("ReadSettings(%s) error = %v", legacy, err)
		}
		if got.Path != SettingsFile || got.Legacy != nil || got.LegacyWarning() != "" {
			t.Fatalf("ReadSettings(%s) = %+v, want the settings file and no warning", legacy, got)
		}
		settings, err := got.Settings()
		if err != nil {
			t.Fatalf("Settings() error = %v", err)
		}
		want := Settings{
			Schema:   SettingsSchemaID,
			Sources:  map[string]string{"x": "https://github.com/discobox-ai/x"},
			Features: map[string]Feature{"brew": {Enabled: false}, "nix": {Enabled: true}, "go": {Enabled: true}},
			ProjectLayer: ProjectLayer{
				RunCommand:              []string{"make", "dev"},
				WorkingDirectorySubpath: "app",
				FilesAdd:                []File{{Path: ".config/a", Content: "b"}},
			},
		}
		if !reflect.DeepEqual(settings, want) {
			t.Fatalf("Settings() = %+v, want %+v", settings, want)
		}
	}
}

// Without settings.json, each reader reads the old file it read before, and
// is told to warn about every old file there.
func TestReadSettingsFallsBackToTheLegacyFiles(t *testing.T) {
	read := repository(map[string]string{
		LegacySourcesFile: `{"x": "https://github.com/discobox-ai/x"}`,
		LegacyProjectFile: `{"runCommand": ["make"]}`,
	})
	sources, err := ReadSettings(read, LegacySourcesFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := sources.Settings(); err != nil || got.Sources["x"] == "" || got.RunCommand != nil {
		t.Fatalf("the client's settings = %+v, %v; want sources.json's sources alone", got, err)
	}
	project, err := ReadSettings(read, LegacyProjectFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := project.Settings(); err != nil || got.Sources != nil || !slices.Equal(got.RunCommand, []string{"make"}) {
		t.Fatalf("the pool's settings = %+v, %v; want project.json's project layer alone", got, err)
	}
	for _, read := range []RepositorySettings{sources, project} {
		if !slices.Equal(read.Legacy, []string{LegacySourcesFile, LegacyProjectFile}) ||
			!strings.Contains(read.LegacyWarning(), "sources.json and .discobox/project.json are read") {
			t.Fatalf("legacy = %v (%q), want both old files named", read.Legacy, read.LegacyWarning())
		}
	}

	// The other reader's old file alone still earns the warning.
	only := repository(map[string]string{LegacyProjectFile: `{}`})
	got, err := ReadSettings(only, LegacySourcesFile)
	if err != nil || got.Path != "" || !strings.Contains(got.LegacyWarning(), LegacyProjectFile+" is read") {
		t.Fatalf("ReadSettings = %+v, %v; want nothing read and project.json warned about", got, err)
	}
	if settings, err := got.Settings(); err != nil || !reflect.DeepEqual(settings, Settings{}) {
		t.Fatalf("Settings() = %+v, %v; want none", settings, err)
	}
}

// A repository holding both kinds is refused rather than merged.
func TestReadSettingsRefusesBothKinds(t *testing.T) {
	for _, legacy := range []string{LegacySourcesFile, LegacyProjectFile} {
		read := repository(map[string]string{SettingsFile: `{}`, legacy: `{}`})
		for _, reader := range []string{LegacySourcesFile, LegacyProjectFile} {
			_, err := ReadSettings(read, reader)
			if !errors.Is(err, ErrSettingsConflict) || !strings.Contains(err.Error(), legacy) {
				t.Fatalf("settings.json beside %s, read for %s: error = %v, want a conflict naming it", legacy, reader, err)
			}
		}
	}
}

func TestReadSettingsNamesEveryFileItReplaces(t *testing.T) {
	read := repository(map[string]string{SettingsFile: `{}`, LegacySourcesFile: `{}`, LegacyProjectFile: `{}`})
	_, err := ReadSettings(read, LegacySourcesFile)
	if !errors.Is(err, ErrSettingsConflict) || !strings.Contains(err.Error(), LegacySourcesFile+" and "+LegacyProjectFile) {
		t.Fatalf("error = %v, want both old files named", err)
	}
}

func TestReadSettingsPassesOnAFailedRead(t *testing.T) {
	broken := errors.New("disk on fire")
	_, err := ReadSettings(func(string) ([]byte, error) { return nil, broken }, LegacySourcesFile)
	if !errors.Is(err, broken) {
		t.Fatalf("error = %v, want the read's", err)
	}
}

func TestFeatureIsABooleanOrAnObject(t *testing.T) {
	for input, want := range map[string]bool{
		`true`:                         true,
		`false`:                        false,
		`{}`:                           true,
		`{"enabled": false}`:           false,
		`{"enabled": true, "x": 1}`:    true,
		`{"version": "1.2", "y": [1]}`: true,
	} {
		var feature Feature
		if err := json.Unmarshal([]byte(input), &feature); err != nil || feature.Enabled != want {
			t.Fatalf("%s: feature = %+v, %v; want enabled %v", input, feature, err, want)
		}
	}
	for _, input := range []string{`null`, `"off"`, `0`, `[]`, `{"enabled": "no"}`} {
		var feature Feature
		if err := json.Unmarshal([]byte(input), &feature); err == nil {
			t.Fatalf("%s: feature = %+v, want an error", input, feature)
		}
	}
}

// A malformed file is named in what is said about it.
func TestSettingsNamesAMalformedFile(t *testing.T) {
	for _, tc := range []struct{ path, data string }{
		{SettingsFile, `{"features": {"brew": "off"}}`},
		{LegacySourcesFile, `["x"]`},
		{LegacyProjectFile, `{not json`},
	} {
		if _, err := (RepositorySettings{Path: tc.path, Data: []byte(tc.data)}).Settings(); err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Fatalf("%s %s: error = %v, want one naming the file", tc.path, tc.data, err)
		}
	}
	// A key this reader does not define is read past, and named for a warning.
	read := RepositorySettings{Path: SettingsFile, Data: []byte(`{"sources": {"x": "https://github.com/discobox-ai/x"}, "skills": [], "RunCommand": ["make"]}`)}
	settings, err := read.Settings()
	if err != nil || settings.Sources["x"] == "" || !slices.Equal(settings.RunCommand, []string{"make"}) {
		t.Fatalf("settings with a key it does not define = %+v, %v; want the rest read", settings, err)
	}
	if unknown := read.UnknownKeys(); len(unknown) != 1 || string(unknown["skills"]) != "[]" {
		t.Fatalf("unknown keys = %v, want skills alone (keys match without regard to case)", unknown)
	}
	if unknown := (RepositorySettings{Path: SettingsFile, Data: []byte(`{}`)}).UnknownKeys(); unknown != nil {
		t.Fatalf("an empty file's unknown keys = %v", unknown)
	}
	// The legacy project layer keeps its tolerant decode.
	if _, err := (RepositorySettings{Path: LegacyProjectFile, Data: []byte(`{"someday": 1}`)}).Settings(); err != nil {
		t.Fatalf("an unknown key in project.json: %v, want it ignored as before", err)
	}
}

// This repository's own settings are read the way any other's are.
func TestThisRepositorysSettingsAreRead(t *testing.T) {
	root := filepath.Join("..", "..")
	read, err := ReadSettings(func(path string) ([]byte, error) {
		return os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	}, LegacySourcesFile)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := read.Settings()
	if err != nil || read.Path != SettingsFile || settings.Schema != SettingsSchemaID || len(settings.Sources) == 0 {
		t.Fatalf("this repository's settings = %+v (%s), %v; want its settings.json with its schema and sources", settings, read.Path, err)
	}
}
