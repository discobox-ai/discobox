package cli

import (
	"bytes"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func updateTestConfig() *apimodel.HarnessConfig {
	return &apimodel.HarnessConfig{
		ID: "hc_1", Name: "Claude", Revision: apiclientgen.NewOptInt64(3),
		Files: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".claude.json", Content: "{}", CreateOnly: apiclientgen.NewOptBool(true)},
			{Path: ".claude/settings.json", Content: "{}"},
		}),
		FileOverrides: apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
			{Path: ".config/a.txt", Content: "a"},
			{Path: ".config/b.txt", Content: "b"},
		}),
	}
}

func parsedUpdateBody(t *testing.T, args ...string) (*apimodel.UpdateHarnessConfigBody, error) {
	t.Helper()
	cmd := (&App{}).newHarnessUpdateCommand()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	var opts harnessUpdateOptions
	opts.files, _ = cmd.Flags().GetStringArray("file")
	opts.createOnlyFile, _ = cmd.Flags().GetStringArray("create-only-file")
	opts.removeFiles, _ = cmd.Flags().GetStringArray("remove-file")
	return updateHarnessBody(cmd, opts, updateTestConfig())
}

func sentOverrides(t *testing.T, body *apimodel.UpdateHarnessConfigBody) []string {
	t.Helper()
	files, sent := body.FileOverrides.Get()
	if !sent {
		return nil
	}
	out := []string{}
	for _, f := range files {
		entry := f.Path + "=" + f.Content
		if f.CreateOnly.Or(false) {
			entry += " (create-only)"
		}
		out = append(out, entry)
	}
	return out
}

// --clear-file sends an empty set of a person's files, which is what empties
// it; leaving both flags out sends none. The bundle's files are never sent.
func TestHarnessUpdateClearFileSendsAnEmptyList(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sent bool
	}{
		{args: nil, sent: false},
		{args: []string{"--clear-file"}, sent: true},
	} {
		body, err := parsedUpdateBody(t, tc.args...)
		if err != nil {
			t.Fatal(err)
		}
		files, sent := body.FileOverrides.Get()
		if sent != tc.sent || len(files) != 0 {
			t.Fatalf("%v: files = %v sent %v, want sent %v and empty", tc.args, files, sent, tc.sent)
		}
		if body.Revision != 3 {
			t.Fatalf("revision = %d, want the one read", body.Revision)
		}
	}
}

// --file adds or replaces one path in a person's set and keeps every other
// (#524); --remove-file takes one out.
func TestHarnessUpdateFileChangesOnlyThePathsItNames(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--file", ".config/x.txt=x"}, ".config/a.txt=a|.config/b.txt=b|.config/x.txt=x"},
		{[]string{"--file", ".config/a.txt=new"}, ".config/a.txt=new|.config/b.txt=b"},
		{[]string{"--file", ".claude.json={}", "--create-only-file", ".claude.json"}, ".config/a.txt=a|.config/b.txt=b|.claude.json={} (create-only)"},
		{[]string{"--remove-file", ".config/a.txt"}, ".config/b.txt=b"},
		{[]string{"--remove-file", ".config/a.txt", "--file", ".config/c.txt=c"}, ".config/b.txt=b|.config/c.txt=c"},
	} {
		body, err := parsedUpdateBody(t, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if got := strings.Join(sentOverrides(t, body), "|"); got != tc.want {
			t.Fatalf("%v: sent %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestHarnessUpdateRefusesARemovalItCannotMake(t *testing.T) {
	for _, args := range [][]string{
		{"--remove-file", ".claude.json"},
		{"--remove-file", ".config/a.txt", "--file", ".config/a.txt=again"},
		{"--clear-file", "--remove-file", ".config/a.txt"},
	} {
		if _, err := parsedUpdateBody(t, args...); err == nil {
			t.Fatalf("%v: want refused", args)
		}
	}
}

// harnesses get lists each path once with the set it comes from, the one an
// override hides, and a saved path escaped: a discobox's saved harness change
// wrote it.
func TestWriteHarnessListsEachFileWithItsSource(t *testing.T) {
	cfg := updateTestConfig()
	cfg.FileOverrides = apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
		{Path: ".claude/settings.json", Content: "mine"},
		{Path: ".config/x.txt", Content: "x"},
	})
	cfg.ConfiguredFiles = apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{
		{Path: ".claude/settings.json", Content: "configured"},
		{Path: "evil\x1b]0;pwned\x07", Content: "x"},
	})
	cmd := (&App{}).newHarnessUpdateCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := (&App{}).writeHarness(cmd, cfg); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Fatalf("output carries a raw escape:\n%q", got)
	}
	var rows []string
	for _, line := range strings.Split(got, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && strings.HasPrefix(fields[0], ".") {
			rows = append(rows, strings.Join(fields, " "))
		}
	}
	want := []string{
		".claude/settings.json yours (over saved) no",
		".config/x.txt yours no",
		".claude.json bundle yes",
	}
	if strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Fatalf("file rows = %q, want %q\n%s", rows, want, got)
	}
}
