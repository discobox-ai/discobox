package sandboxconfig

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
)

func TestSkillsValidate_AcceptsASkillWithFiles(t *testing.T) {
	skills := Skills{"foo": {Skill: "# foo", Files: []SkillFile{
		{Path: "scripts/run.sh", Content: []byte("#!/bin/sh\n"), Executable: true},
		{Path: "reference/notes.md", Content: []byte("notes")},
	}}}
	if err := skills.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestSkillsValidate_Refuses(t *testing.T) {
	file := func(p string) Skills {
		return Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: p}}}}
	}
	cases := map[string]Skills{
		"empty name":           {"": {Skill: "x"}},
		"name with slash":      {"a/b": {Skill: "x"}},
		"name with backslash":  {`a\b`: {Skill: "x"}},
		"dot name":             {".": {Skill: "x"}},
		"dot-dot name":         {"..": {Skill: "x"}},
		"hidden name":          {".foo": {Skill: "x"}},
		"empty SKILL.md":       {"foo": {Skill: " \n"}},
		"empty path":           file(""),
		"absolute path":        file("/etc/passwd"),
		"escaping path":        file("../bar/SKILL.md"),
		"inner dot-dot":        file("a/../../b"),
		"unclean path":         file("./a"),
		"trailing slash":       file("a/"),
		"backslash path":       file(`a\b`),
		"SKILL.md as a file":   file(SkillFileName),
		"dot path":             file("."),
		"duplicate path":       {"foo": {Skill: "x", Files: []SkillFile{{Path: "a"}, {Path: "a"}}}},
		"file is a directory":  {"foo": {Skill: "x", Files: []SkillFile{{Path: "a"}, {Path: "a/b"}}}},
		"over the size limit":  {"foo": {Skill: strings.Repeat("x", MaxSkillsBytes+1)}},
		"over the limit, sums": {"a": {Skill: strings.Repeat("x", MaxSkillsBytes/2+1)}, "b": {Skill: strings.Repeat("x", MaxSkillsBytes/2)}},
		"paths over the limit": {"foo": {Skill: "x", Files: []SkillFile{{Path: strings.Repeat("a", MaxSkillsBytes)}}}},
		"too many files":       manyFiles(MaxSkillFiles),
		"too many skills":      manySkills(MaxSkillFiles + 1),
		"superscript device":   {"COM¹": {Skill: "x"}},
		"console input device": {"conin$": {Skill: "x"}},
		"console output path":  file("CONOUT$.txt"),
		"long whole path":      file(strings.Repeat("abcdefg/", MaxSkillPathBytes/8) + "x"),
		"superscript path":     file("bin/lpt².txt"),
		"long name":            {strings.Repeat("a", MaxSkillSegmentBytes+1): {Skill: "x"}},
		"long path segment":    file("a/" + strings.Repeat("b", MaxSkillSegmentBytes+1)),
		"windows character":    {"a:b": {Skill: "x"}},
		"windows path char":    file("a/b?.md"),
		"control character":    file("a\x01b"),
		"NUL in a name":        {"a\x00b": {Skill: "x"}},
		"trailing dot":         {"foo.": {Skill: "x"}},
		"trailing space":       file("a /b"),
		"device name":          {"con": {Skill: "x"}},
		"device path":          file("bin/NUL.txt"),
		"names differ in case": {"Foo": {Skill: "x"}, "foo": {Skill: "x"}},
		"names fold together":  {"Σa": {Skill: "x"}, "ςa": {Skill: "x"}},
		"paths fold together":  {"foo": {Skill: "x", Files: []SkillFile{{Path: "Kelvin"}, {Path: "\u212aelvin"}}}},
		"paths differ in case": {"foo": {Skill: "x", Files: []SkillFile{{Path: "Bin"}, {Path: "bin/run.sh"}}}},
		"skill.md in any case": file("skill.md"),
		"under SKILL.md":       file("SKILL.md/notes.md"),
		"under skill.md":       file("skill.md/x"),
	}
	for name, skills := range cases {
		t.Run(name, func(t *testing.T) {
			if err := skills.Validate(); err == nil {
				t.Fatal("Validate() = nil, want a refusal")
			}
		})
	}
}

func TestSkillsSizeCountsEveryFile(t *testing.T) {
	skills := Skills{
		"a": {Skill: "12345", Files: []SkillFile{{Path: "x", Content: []byte("123")}}},
		"b": {Skill: "12"},
	}
	// 5 + 2 of SKILL.md, 3 of content, and 1 of the path "x".
	if got := skills.Size(); got != 11 {
		t.Fatalf("Size() = %d, want 11", got)
	}
	if got := skills.Names(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Names() = %v, want [a b]", got)
	}
}

// Content is bytes, so a skill's images and binaries survive the bootstrap's
// JSON as base64.
func TestSkillFileContentRoundTripsAsBase64(t *testing.T) {
	in := Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: "logo.png", Content: []byte{0x89, 'P', 'N', 'G', 0, 0xff}}}}}
	encoded, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"content":"iVBORwD/"`) {
		t.Fatalf("encoded = %s, want base64 content", encoded)
	}
	var out Skills
	if err := json.Unmarshal(encoded, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

func TestEffective_SkillsAreRuntimeOnlyAndCopied(t *testing.T) {
	doc := Document{Runtime: RuntimeLayer{Skills: Skills{"foo": {Skill: "# foo", Files: []SkillFile{{Path: "a", Content: []byte("a")}}}}}}
	cfg, _ := Effective(doc)
	if !reflect.DeepEqual(cfg.Skills, doc.Runtime.Skills) {
		t.Fatalf("Skills = %+v, want the runtime layer's", cfg.Skills)
	}
	cfg.Skills["foo"].Files[0].Content[0] = 'b'
	if doc.Runtime.Skills["foo"].Files[0].Content[0] != 'a' {
		t.Fatal("Effective's Skills share content with the document")
	}
	// Once in the bootstrap, not twice: provenance leaves them out.
	if _, prov := Effective(doc); prov.Runtime.Skills != nil {
		t.Fatalf("provenance skills = %+v, want none", prov.Runtime.Skills)
	}
	if cfg, _ := Effective(Document{}); cfg.Skills != nil {
		t.Fatalf("Skills = %+v, want nil when the runtime layer has none", cfg.Skills)
	}
}

func manyFiles(n int) Skills {
	files := make([]SkillFile, n)
	for i := range files {
		files[i].Path = fmt.Sprintf("f%d", i)
	}
	return Skills{"foo": {Skill: "x", Files: files}}
}

func manySkills(n int) Skills {
	skills := make(Skills, n)
	for i := range n {
		skills[fmt.Sprintf("s%d", i)] = Skill{Skill: "x"}
	}
	return skills
}

// Skills at exactly the file limit are taken, each SKILL.md counted, and so is
// a name of the longest a file name may be; names that differ in more than
// case are separate skills.
func TestSkillsValidate_AcceptsTheLimitsAndPortableNames(t *testing.T) {
	if err := manyFiles(MaxSkillFiles - 1).Validate(); err != nil {
		t.Fatalf("%d files and a SKILL.md: %v", MaxSkillFiles-1, err)
	}
	if err := manySkills(MaxSkillFiles).Validate(); err != nil {
		t.Fatalf("%d skills: %v", MaxSkillFiles, err)
	}
	if err := (Skills{strings.Repeat("a", MaxSkillSegmentBytes): {Skill: "x"}}).Validate(); err != nil {
		t.Fatalf("a %d-byte name: %v", MaxSkillSegmentBytes, err)
	}
	skills := Skills{"foo-bar_1.2": {Skill: "x", Files: []SkillFile{{Path: ".env"}, {Path: "con-tents.md"}}}, "Foo2": {Skill: "x"}}
	if err := skills.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

// A skills directory is a clean path inside the repository; it names a
// directory the checkout already has, so it may hold what a Windows file name
// may not.
func TestValidateSkillsDirectory(t *testing.T) {
	for _, dir := range []string{".", "skills", "agents/skills", "odd@name:dir", ".claude/skills"} {
		if err := ValidateSkillsDirectory(dir); err != nil {
			t.Fatalf("ValidateSkillsDirectory(%q) = %v", dir, err)
		}
	}
	for _, dir := range []string{"", "/abs", "..", "../up", "a/../b", "a/", "./a", `a\b`, "a//b", "a\nb"} {
		if err := ValidateSkillsDirectory(dir); err == nil {
			t.Fatalf("ValidateSkillsDirectory(%q) = nil, want a refusal", dir)
		}
	}
}

// A project's skills repository is a remote one, carries no credential, and
// holds nothing Git would read as an option (ADR 26-10-14-894 §1).
//
//nolint:gosec // G101: fixtures of a credential in a URL, which is what is refused.
func TestValidateSkillsRepository(t *testing.T) {
	for _, tc := range []struct {
		url, ref string
		ok       bool
	}{
		{url: "https://github.com/acme/tools", ok: true},
		{url: "https://github.com/acme/tools", ref: "release/v2", ok: true},
		{url: "git@github.com:acme/tools.git", ref: "0123abcd", ok: true},
		{url: "ssh://git@example.com:2222/acme/tools.git", ok: true},
		{url: ""},
		{url: "./tools"},
		{url: "/srv/tools"},
		{url: "file:///srv/tools"},
		{url: "--upload-pack=touch x#@x:y"},
		{url: "https://x-access-token:ghp_x@github.com/acme/tools"},
		{url: "https://ghp_x@github.com/acme/tools"},
		{url: "ssh://git:pw@example.com/acme/tools"},
		{url: "https://github.com/acme//tools"},
		{url: "ssh://-oProxyCommand=x/acme/tools"},
		{url: "ssh://-oProxyCommand=x@example.com/acme/tools"},
		{url: "git@-oProxyCommand=x:acme/tools"},
		{url: "https://github.com/acme/tools\u0085"},
		{url: "https://github.com/acme/tools", ref: "-x"},
		{url: "https://github.com/acme/tools", ref: "."},
		{url: "https://github.com/acme/tools", ref: "a..b"},
		{url: "https://github.com/acme/tools", ref: "a@b"},
		{url: "https://github.com/acme/tools", ref: "a/.b"},
		{url: "https://github.com/acme/tools", ref: "a.lock"},
		{url: "https://github.com/acme/tools", ref: "a b"},
	} {
		if err := ValidateSkillsRepository(tc.url, tc.ref); (err == nil) != tc.ok {
			t.Errorf("ValidateSkillsRepository(%q, %q) = %v, want ok %v", tc.url, tc.ref, err, tc.ok)
		}
	}
}

// Only a skills source may be placed below home, and a skills source is
// placed below home or at a clean absolute path (ADR 26-11-01-692 §1).
func TestValidateSourceDirectory(t *testing.T) {
	linux := sandboxpath.For(platform.Platform{OS: "linux"})
	windows := sandboxpath.For(platform.Platform{OS: "windows"})
	for _, tc := range []struct {
		name      string
		directory string
		skills    bool
		rules     []sandboxpath.Paths
		want      string
	}{
		{"skills below home", "~/.discobox/skill-sources/tools", true, []sandboxpath.Paths{linux}, ""},
		{"skills absolute, as before", "/workspace/skill-sources/tools", true, []sandboxpath.Paths{linux}, ""},
		{"skills absolute on windows", `C:\workspace\skill-sources\tools`, true, []sandboxpath.Paths{linux, windows}, ""},
		{"no directory", "", true, nil, ""},
		{"work absolute", "/workspace/app", false, []sandboxpath.Paths{linux}, ""},
		{"work relative is left to placement", "app", false, []sandboxpath.Paths{linux}, ""},
		{"work below home", "~/app", false, []sandboxpath.Paths{linux}, "only a skills source"},
		{"skills climbing out of home", "~/../etc", true, []sandboxpath.Paths{linux}, "clean relative path below home"},
		{"another user's home", "~root/x", true, []sandboxpath.Paths{linux}, "clean relative path below home"},
		{"skills relative", "skill-sources/tools", true, []sandboxpath.Paths{linux}, "neither below home"},
		{"skills unclean", "/workspace/../etc", true, []sandboxpath.Paths{linux}, "neither below home"},
		{"skills unclean, no rules", "/workspace/../etc", true, nil, ""},
		{"skills climbing out of home, no rules", "~/../etc", true, nil, "clean relative path below home"},
	} {
		err := ValidateSourceDirectory(tc.directory, tc.skills, tc.rules...)
		if tc.want == "" && err != nil {
			t.Errorf("%s: ValidateSourceDirectory(%q) = %v, want accepted", tc.name, tc.directory, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: ValidateSourceDirectory(%q) = %v, want an error naming %q", tc.name, tc.directory, err, tc.want)
		}
	}
}
